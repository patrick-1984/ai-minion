//go:build e2e && unix

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// workerPath is the PATH for a Worker started by a test: the system
// directories plus a private directory with the tmux the test run uses,
// without any directory that holds a coding harness (claude, codex,
// opencode, ...). A harness installed on the host would otherwise be probed
// by the Worker's inventory and could keep writing into the test's home
// after the test ends.
func workerPath(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	if tmux, err := exec.LookPath("tmux"); err == nil {
		if err := os.Symlink(tmux, filepath.Join(bin, "tmux")); err != nil {
			t.Fatal(err)
		}
	}
	harnesses := []string{"claude", "codex", "gemini", "opencode", "aider", "qwen", "cline"}
	dirs := []string{bin}
	for _, d := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		clean := true
		for _, h := range harnesses {
			if _, err := os.Stat(filepath.Join(d, h)); err == nil {
				clean = false
				break
			}
		}
		if clean {
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
