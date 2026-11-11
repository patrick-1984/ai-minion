//go:build !windows

package identity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func savedKey(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id.key")
	if err := SaveKey(p, mustKey(t)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadKeyRefusesASymlinkEvenToAPrivateKey(t *testing.T) {
	target := savedKey(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := LoadKey(link)
	if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	if _, err := LoadKey(target); err != nil {
		t.Fatalf("the key itself must load: %v", err)
	}
}

func TestLoadKeyRefusesAKeyOfAnotherOwner(t *testing.T) {
	p := savedKey(t)
	old := euid
	euid = func() int { return os.Geteuid() + 4242 }
	defer func() { euid = old }()
	_, err := LoadKey(p)
	if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadKeyRefusesGroupOrOtherAccessAndKeepsMissingAsNotExist(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o777} {
		p := savedKey(t)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(p); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("mode %04o: err = %v", mode, err)
		}
	}
	if _, err := LoadKey(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
}

func TestLoadKeyRefusesADirectory(t *testing.T) {
	if _, err := LoadKey(t.TempDir()); err == nil {
		t.Error("a directory was accepted")
	}
}
