//go:build windows && psmux

package tmux

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// Paste: where does a buffer land, and can another session paste it?
	in := func(stdin string, args ...string) {
		cmd := exec.Command(bin, append([]string{"-S", sock}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		t.Logf("psmux %v <stdin> -> err=%v out=%q", args, err, out)
	}
	in("echo AAA-one", "load-buffer", "-b", "mb1", "-")
	run("list-buffers")
	run("paste-buffer", "-d", "-p", "-r", "-b", "mb1", "-t", "=alpha:0.0")
	in("echo BBB-two", "load-buffer", "-b", "mb2", "-", ";", "paste-buffer", "-d", "-p", "-r", "-b", "mb2", "-t", "=alpha:0.0")
	in("echo CCC-three", "load-buffer", "-t", "=alpha:", "-b", "mb3", "-")
	run("paste-buffer", "-d", "-p", "-r", "-b", "mb3", "-t", "=alpha:0.0")
	run("send-keys", "-t", "=alpha:0.0", "-l", "DDD-four")
	run("if-shell", "-F", "-t", "=alpha:0.0", "1", "display-message -p yes-alpha", "display-message -p no")
	time.Sleep(time.Second)
	run("capture-pane", "-p", "-t", "=alpha:0.0")
	run("capture-pane", "-p", "-t", "=beta:0.0")
	run("display-message", "-p", "-t", "=alpha:0.0", "#{session_created} #{pane_current_command} #{pane_in_mode} #{pane_input_off} #{synchronize-panes} #{session_id}")
	run("show-options", "-gqv", "default-shell")
	runL("list-sessions")
	runL("-V")
	run("-V")
}
