//go:build !windows

package version

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCheckTrustedDir(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, mode os.FileMode) string {
		p := filepath.Join(root, name)
		mustMkdir(t, p, mode)
		return p
	}
	if err := checkTrustedDir(mk("private", 0o700)); err != nil {
		t.Errorf("private directory: %v", err)
	}
	if err := checkTrustedDir(mk("readable", 0o755)); err != nil {
		t.Errorf("a directory others can only read: %v", err)
	}
	for name, mode := range map[string]os.FileMode{
		"group-writable": 0o770,
		"world-writable": 0o777,
		"sticky-final":   0o777 | os.ModeSticky, // a sticky directory is fine above, never as the cache itself
	} {
		if err := checkTrustedDir(mk(name, mode)); !errors.Is(err, ErrUntrustedCache) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A sticky directory above a private one is how /tmp looks.
	sticky := mk("sticky", 0o777|os.ModeSticky)
	child := filepath.Join(sticky, "cache")
	mustMkdir(t, child, 0o700)
	if err := checkTrustedDir(child); err != nil {
		t.Errorf("below a sticky directory: %v", err)
	}
	// A plain world-writable directory above it is not.
	open := mk("open", 0o777)
	child = filepath.Join(open, "cache")
	mustMkdir(t, child, 0o700)
	if err := checkTrustedDir(child); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("below a world-writable directory: %v", err)
	}
	// A link in a world-writable directory is a hop others can redirect, even
	// when its target is private.
	link := filepath.Join(open, "link")
	if err := os.Symlink(mk("target", 0o700), link); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedDir(link); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("through a link in a world-writable directory: %v", err)
	}
	// A link in a private directory to a private directory is fine.
	good := filepath.Join(mk("linkdir", 0o700), "link")
	if err := os.Symlink(filepath.Join(root, "private"), good); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedDir(good); err != nil {
		t.Errorf("trusted link: %v", err)
	}
	loop := filepath.Join(root, "linkdir", "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedDir(loop); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("link loop: %v", err)
	}
	if err := checkTrustedDir(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing directory: %v", err)
	}
	// A file is not a directory.
	f := filepath.Join(root, "file")
	writeFile(t, f, "x", 0o600)
	if err := checkTrustedDir(f); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("a file: %v", err)
	}
}

func TestOpenTrusted(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		writeFile(t, p, "data", mode)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, name := range []string{"private", "exec"} {
		mode := os.FileMode(0o600)
		if name == "exec" {
			mode = 0o755
		}
		f, err := openTrusted(write(name, mode))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = f.Close()
	}
	for name, mode := range map[string]os.FileMode{"group": 0o660, "world": 0o666} {
		if _, err := openTrusted(write(name, mode)); !errors.Is(err, ErrUntrustedCache) {
			t.Errorf("%s-writable file: %v", name, err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(write("real", 0o600), link); err != nil {
		t.Fatal(err)
	}
	if _, err := openTrusted(link); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("symbolic link: %v", err)
	}
	if _, err := openTrusted(dir); !errors.Is(err, ErrUntrustedCache) {
		t.Errorf("directory: %v", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openTrusted(fifo); !errors.Is(err, ErrUntrustedCache) { // must not block
		t.Errorf("fifo: %v", err)
	}
	if _, err := openTrusted(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

// TestCheckNewFileAndInstallDir: a file the updater just created must not be
// writable by group or others, and the install directory is not refused for a
// group-writable mode (/usr/local/bin is often root:staff 2775).
func TestCheckNewFileAndInstallDir(t *testing.T) {
	dir := t.TempDir()
	for name, wantErr := range map[string]bool{"private": false, "world": true} {
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if wantErr {
			if err := f.Chmod(0o666); err != nil {
				t.Fatal(err)
			}
		}
		err = checkNewFile(f)
		if wantErr != errors.Is(err, ErrUntrustedCache) || (!wantErr && err != nil) {
			t.Errorf("%s: err = %v, want untrusted=%v", name, err, wantErr)
		}
		_ = f.Close()
	}

	bin := filepath.Join(dir, "bin")
	mustMkdir(t, bin, 0o775)
	if err := os.Chmod(bin, 0o775); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(bin, "minion")
	writeFile(t, target, "old", 0o755)
	src := filepath.Join(dir, "src")
	writeFile(t, src, "new", 0o600)
	if err := ReplaceBinary(target, src, "linux", sha256Hex("new")); err != nil {
		t.Fatalf("install into a group-writable directory: %v", err)
	}
	if got := readFile(t, target); got != "new" {
		t.Errorf("target = %q", got)
	}
}
