//go:build !windows

package cli

import (
	"os"
	"testing"
)

// makePermissive lets every account read path.
func makePermissive(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}
