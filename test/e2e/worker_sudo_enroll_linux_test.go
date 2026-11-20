//go:build e2e && linux

package e2e

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Opt in on a machine with passwordless sudo. Only --configure-only is used:
// every working file is in the test directory and no service is started.
func TestSudoProvisionKeepsTargetIdentityOnRetry(t *testing.T) {
	if os.Getenv("MINION_TEST_SUDO") != "1" || os.Geteuid() == 0 {
		t.Skip("set MINION_TEST_SUDO=1 as a user with sudo -n to exercise the UID change")
	}
	owner, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	original, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.SaveKey(filepath.Join(state, agent.WorkerKeyFile), original); err != nil {
		t.Fatal(err)
	}
	h := newInviteHub(t)
	key := h.newKey(t, ops.InvitationOneTime)
	token, err := identity.NewJoinToken(h.url(), key)
	if err != nil {
		t.Fatal(err)
	}
	bin := minionBinary(t)
	up := func(in string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		base := []string{"-n", "env", "-i", "PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "SUDO_USER=" + owner.Username,
			bin, "worker", "up", "--configure-only", "--share-existing", "none", "--state-dir", state, "--tmux-socket", filepath.Join(dir, "tmux.sock")}
		cmd := exec.CommandContext(ctx, "sudo", append(base, args...)...)
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), key.String()) || strings.Contains(string(out), token.Encode()) {
			t.Fatal("enrollment helper disclosed the invitation")
		}
		if err != nil {
			t.Fatalf("sudo provision: %v\n%s", err, out)
		}
	}
	up(token.Encode()+"\n", "--join", "-")
	// This path must never be opened: this Worker is already enrolled.
	up("", "--invite-key", filepath.Join(dir, "absent"))
	saved, err := identity.LoadKey(filepath.Join(state, agent.WorkerKeyFile))
	if err != nil || saved.Fingerprint() != original.Fingerprint() {
		t.Fatalf("the target's original private identity was not preserved: %v", err)
	}
	rows, err := h.st.ListWorkers(t.Context())
	if err != nil || len(rows) != 1 || !rows[0].Pending {
		t.Fatalf("expected one pending Worker, got %d: %v", len(rows), err)
	}
}
