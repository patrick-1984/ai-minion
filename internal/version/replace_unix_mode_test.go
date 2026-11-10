//go:build !windows

package version

import (
	"os"
	"path/filepath"
	"testing"
)

// A legacy target that is group/other writable must not make the freshly
// installed, digest-verified executable writable by group/other.
func TestReplaceBinaryStripsGroupOtherWrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "minion")
	next := filepath.Join(dir, "new")
	writeFile(t, target, "old", 0o600)
	writeFile(t, next, "new", 0o600)
	if err := os.Chmod(target, 0o777); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceBinary(target, next, "linux", sha256Hex("new")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "new" {
		t.Fatalf("target = %q", got)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o755 {
		t.Errorf("installed mode = %v, want 0755 (no group/other write)", perm)
	}
}
