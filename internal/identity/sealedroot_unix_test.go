//go:build unix

package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealedRootFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "root.key")
	k, _ := GenerateKey()
	if err := SaveSealedRootKey(path, k.key(), testPass); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealedRootSigner(path, testPass); err == nil {
		t.Fatal("world-readable file accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealedRootSigner(link, testPass); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := LoadSealedRootSigner(path, testPass); err != nil {
		t.Fatal(err)
	}
}
