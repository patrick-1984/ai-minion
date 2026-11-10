//go:build !windows

package identity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveKeyMode0600(t *testing.T) {
	// Even with a permissive umask the file is created 0600.
	old := umask(0)
	defer umask(old)
	path := filepath.Join(t.TempDir(), "id.key")
	if err := SaveKey(path, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o, want 0600", fi.Mode().Perm())
	}
}

func TestSaveKeyReplacesPermissiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.key")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveKey(path, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o", fi.Mode().Perm())
	}
}

func TestLoadKeyRefusesGroupOrWorldAccess(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o610, 0o601, 0o700 | 0o070} {
		path := filepath.Join(t.TempDir(), "id.key")
		if err := SaveKey(path, mustKey(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(path); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("mode %04o: err = %v", mode, err)
		}
	}
	// 0400 and 0600 are accepted.
	for _, mode := range []os.FileMode{0o400, 0o600} {
		path := filepath.Join(t.TempDir(), "id.key")
		_ = SaveKey(path, mustKey(t))
		_ = os.Chmod(path, mode)
		if _, err := LoadKey(path); err != nil {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
}

func TestCheckPrivateRejectsNonRegular(t *testing.T) {
	if err := CheckPrivate(t.TempDir()); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dir: %v", err)
	}
	if err := CheckPrivate(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
}

func TestWritePrivateFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	// target is a non-empty directory: rename fails
	target := filepath.Join(dir, "t")
	if err := os.MkdirAll(filepath.Join(target, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(target, []byte("data")); err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestWritePrivateMissingDir(t *testing.T) {
	if err := WritePrivate(filepath.Join(t.TempDir(), "no", "such", "f"), nil); err == nil {
		t.Fatal("expected error")
	}
}
