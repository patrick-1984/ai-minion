//go:build windows && psmux

package tmux

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPsmuxTargetProbe records how psmux resolves pane targets across two
// sessions (A21 design probe; it logs, it does not assert).
func TestPsmuxTargetProbe(t *testing.T) {
	bin, err := exec.LookPath("psmux")
	if err != nil {
		t.Skip(err)
	}
	sock := filepath.Join(t.TempDir(), "probe.sock")
	run := func(args ...string) {
		out, err := exec.Command(bin, append([]string{"-S", sock}, args...)...).CombinedOutput()
		t.Logf("psmux %v -> err=%v out=%q", args, err, out)
	}
	runL := func(args ...string) {
		out, err := exec.Command(bin, args...).CombinedOutput()
		t.Logf("psmux(default) %v -> err=%v out=%q", args, err, out)
	}
	run("new-session", "-d", "-s", "alpha")
	run("new-session", "-d", "-s", "beta")
	t.Cleanup(func() { run("kill-session", "-t", "alpha"); run("kill-session", "-t", "beta") })
	run("list-panes", "-a", "-F", "#{session_name} #{pane_id} #{window_index}.#{pane_index} #{pane_pid}")
	for _, tgt := range []string{"%1", "=alpha:%1", "=beta:%1", "alpha:%1", "beta:%1", "=alpha:0.0", "=beta:0.0", "=beta:", "=beta"} {
		run("display-message", "-p", "-t", tgt, "#{session_name} #{pane_id}")
	}
	run("display-message", "-p", "-t", "=beta:0.0", "#{session_name}", ";", "display-message", "-p", "-t", "=alpha:0.0", "#{session_name}")
	runL("list-sessions")
	runL("-V")
	run("-V")
}
