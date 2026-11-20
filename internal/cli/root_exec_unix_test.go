//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A binary of a normal user (or under a world-writable directory) is refused
// for a Worker of root; a root-owned system binary passes.
func TestCheckRootExecutable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "minion")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkRootExecutable(bin); err == nil {
		t.Fatal("a binary in a temporary directory was accepted")
	}
	for _, p := range []string{"/bin/sh", "/usr/bin/env"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			if err := checkRootExecutable(p); err != nil {
				t.Errorf("%s: %v", p, err)
			}
			return
		}
	}
}
