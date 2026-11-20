//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// Exercise the same binary, one-use enrollment, control socket and login
// scripts that an operator uses in a workspace without systemd.
func TestLinuxLoginWorkerOnboarding(t *testing.T) {
	if service.Detect(service.SystemEnv()).Manager != service.ManagerNone {
		t.Skip("requires a Linux host without systemd")
	}
	h := newInviteHub(t)
	key := h.newKey(t, ops.InvitationOneTime)
	token, err := identity.NewJoinToken(h.url(), key)
	if err != nil {
		t.Fatal(err)
	}
	bin := minionBinary(t)
	dir, err := os.MkdirTemp("", "mn-login-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	state := filepath.Join(dir, "state")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "XDG_RUNTIME_DIR=" + dir,
		"MINION_STATE_DIR=" + state, "MINION_TMUX_SOCKET=" + filepath.Join(dir, "tmux.sock")}
	cmd := func(args ...string) *exec.Cmd {
		c := exec.CommandContext(t.Context(), bin, append([]string{"worker"}, args...)...)
		c.Env = env
		return c
	}
	run := func(in string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, bin, append([]string{"worker"}, args...)...)
		c.Env = env
		c.Stdin = strings.NewReader(in)
		out, err := c.CombinedOutput()
		if strings.Contains(string(out), token.Encode()) || strings.Contains(string(out), key.String()) {
			t.Fatal("output leaked enrollment credential")
		}
		if err != nil {
			t.Fatalf("%s: %v\n%s", args[0], err, out)
		}
		return string(out)
	}
	// Begin in the old foreground configuration. The join must release it.
	fg := cmd("up", "--foreground", "--share-existing", "none")
	if err := fg.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- fg.Wait() }()
	defer func() { _ = fg.Process.Kill() }()
	status := func() agent.StatusReport {
		t.Helper()
		var r agent.StatusReport
		out := run("", "status", "--json")
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("status: %v", err)
		}
		return r
	}
	eventually(t, "foreground worker", func() bool { return status().RunMode == agent.RunModeForeground })
	// Same UIDs on other tests or the workspace must never share our socket:
	// both candidates are inside dir, including for down after a failed up.
	defer func() { _, _ = cmd("down").CombinedOutput() }()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	// The native Hub command opens the token on descriptor 3 so the
	// terminal remains available for choosing the sessions to share.
	join := exec.CommandContext(ctx, "sh", "-c", `MINION_JOIN_FILE=/dev/fd/3 exec "$1" worker up --non-interactive-share all 3<&0 </dev/null`, "sh", bin)
	join.Env = env
	join.Stdin = strings.NewReader(token.Encode() + "\n")
	summary, err := join.CombinedOutput()
	if strings.Contains(string(summary), key.String()) || strings.Contains(string(summary), token.Encode()) {
		t.Fatal("join leaked the enrollment credential")
	}
	if err != nil {
		t.Fatalf("join: %v: %s", err, summary)
	}
	out := string(summary)
	select {
	case err := <-wait:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old foreground worker still running")
	}
	r := status()
	if !strings.Contains(out, "State: waiting for approval") || !strings.Contains(out, "Hub key (must match the Hub's top bar):") ||
		!strings.Contains(out, key.Fingerprint.Words()) || r.WorkerFingerprintWords == "" || !strings.Contains(out, r.WorkerFingerprintWords) ||
		!strings.Contains(out, "runs as: "+agent.RunModeDetached) || r.RunMode != agent.RunModeDetached {
		t.Fatalf("unexpected startup report: %s", out)
	}
	a, err := workers.NewApprover(workers.ApproverConfig{Store: h.st, Connections: h.service().Registry()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Approve(t.Context(), workers.Actor{ID: "alice"}, r.WorkerID, ""); err != nil {
		t.Fatal(err)
	}
	if out := run("", "up"); !strings.Contains(out, "State: connected") {
		t.Fatal(out)
	}
	run("", "down")
	if status().Connection.Live {
		t.Fatal("worker down left socket live")
	}
	run("", "start")
	eventually(t, "start reconnects the same worker", func() bool { return h.service().Registry().IsOnline(r.WorkerID) })
	run("", "down")
	for _, name := range []string{".profile", ".bashrc"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || strings.Count(string(data), "# Minion Worker login startup") != 1 {
			t.Fatalf("duplicate/missing login hook in %s: %v", name, err)
		}
	}
	login := func() {
		t.Helper()
		c := exec.Command("sh", "-l", "-c", ":")
		c.Env = env[:4] // fresh shell with no MINION settings or credentials
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("login: %v: %s", err, out)
		}
	}
	// A Worker the user stopped stays stopped in this boot: a new shell (a
	// tmux pane) does not bring it back.
	login()
	time.Sleep(2 * time.Second)
	if status().Connection.Live {
		t.Fatal("a login restarted a Worker that was stopped with worker down")
	}
	// After a reboot (the stop mark is for another boot) a login starts it.
	if err := os.WriteFile(filepath.Join(dir, ".config", "minion", "worker-login.stopped"), []byte("another-boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	login()
	eventually(t, "login restarts the approved Worker", func() bool {
		return h.service().Registry().IsOnline(r.WorkerID) && status().RunMode == agent.RunModeDetached
	})
}
