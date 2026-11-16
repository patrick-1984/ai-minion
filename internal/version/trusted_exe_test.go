package version

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckTrustedExecutableRefusesRelativePathsAndDirectories(t *testing.T) {
	if err := CheckTrustedExecutable("psmux.exe"); !errors.Is(err, ErrUntrustedCache) {
		t.Fatalf("relative path: %v", err)
	}
	dir := t.TempDir()
	if err := CheckTrustedExecutable(dir); err == nil {
		t.Fatal("a directory passed as an executable")
	}
	if err := CheckTrustedExecutable(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file passed")
	}
}

func TestCheckTrustedExecutableAcceptsOwnPrivateFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "psmux")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrustedExecutable(p); err != nil {
		t.Fatalf("own file in own temp dir: %v", err)
	}
}
