//go:build e2e && unix

package e2e

import (
	"context"
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The two processes Docker runs: provision with a one-use token on stdin,
// then start a supervised Worker from only the files in the data volume.
func TestDockerProvisionThenRunFromState(t *testing.T) {
	h := newInviteHub(t)
	key := h.newKey(t, ops.InvitationOneTime)
	token, err := identity.NewJoinToken(h.url(), key)
	if err != nil {
		t.Fatal(err)
	}
	bin := minionBinary(t)
	dir, err := os.MkdirTemp("", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, bin, append([]string{"worker"}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "XDG_RUNTIME_DIR=" + dir,
			"MINION_STATE_DIR=" + filepath.Join(dir, "state"), "MINION_TMUX_SOCKET=" + filepath.Join(dir, "tmux.sock")}
		return cmd
	}
	up := command("up", "--configure-only", "--non-interactive-share", "later", "--join", "-")
	up.Stdin = strings.NewReader(token.Encode() + "\n")
	out, err := up.CombinedOutput()
	if strings.Contains(string(out), token.Encode()) || strings.Contains(string(out), key.String()) {
		t.Fatal("worker up printed the credential")
	}
	if err != nil {
		t.Fatalf("worker up: %v\n%s", err, out)
	}
	proc := command("run")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = proc.Process.Signal(os.Interrupt)
		if err := proc.Wait(); err != nil {
			t.Errorf("worker run: %v", err)
		}
	}()
	out, err = command("status", "--wait").CombinedOutput()
	if err != nil {
		t.Fatalf("worker status --wait: %v\n%s", err, out)
	}
	rows, err := h.st.ListWorkers(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Pending {
		t.Fatalf("expected one pending Worker, got %d; err = %v", len(rows), err)
	}
	fp, err := identity.FingerprintOf(ed25519.PublicKey(rows[0].Pub))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"State: waiting for approval", key.Fingerprint.Words(), fp.Words()} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in startup summary:\n%s", want, out)
		}
	}
}
