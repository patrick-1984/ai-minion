package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The replacement keeps a private copy of the old profile; the published
// presence files (owner data of the usage document) are not part of it.
func TestCopyTreeSkipsThePresenceDirectory(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "presence"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"presence/a.json", "presence/a.usage.json", "worker.key"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "worker.key")); err != nil {
		t.Fatal("the key was not copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "presence")); !os.IsNotExist(err) {
		t.Fatalf("presence directory was copied: %v", err)
	}
}
