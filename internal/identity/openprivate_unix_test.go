//go:build !windows

package identity

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenPrivateAcceptsAnOwnerOnlyRegularFile(t *testing.T) {
	f, err := OpenPrivate(writeSecret(t, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 16)
	if _, err := f.Read(b); err != nil || string(b) != "0123456789abcdef" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestOpenPrivateRefusesGroupOrOtherAccess(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o610, 0o601, 0o777} {
		f, err := OpenPrivate(writeSecret(t, mode))
		if err == nil {
			_ = f.Close()
			t.Errorf("mode %04o accepted", mode)
			continue
		}
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("mode %04o: %v is not ErrInvalidArgument", mode, err)
		}
	}
}

func TestOpenPrivateRefusesASymlinkEvenToAPrivateFile(t *testing.T) {
	target := writeSecret(t, 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	f, err := OpenPrivate(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("a symlink was followed")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("err = %v", err)
	}
	if _, err := OpenPrivate(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file must stay os.ErrNotExist, got %v", err)
	}
}

func TestOpenPrivateRefusesAFileOfAnotherOwner(t *testing.T) {
	p := writeSecret(t, 0o600)
	old := euid
	euid = func() int { return os.Geteuid() + 4242 }
	defer func() { euid = old }()
	f, err := OpenPrivate(p)
	if err == nil {
		_ = f.Close()
		t.Fatal("a file owned by another user was accepted")
	}
	if !strings.Contains(err.Error(), "owned by") {
		t.Errorf("err = %v", err)
	}
}

func TestOpenPrivateKeepsReadingTheCheckedFile(t *testing.T) {
	p := writeSecret(t, 0o600)
	f, err := OpenPrivate(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	other := filepath.Join(filepath.Dir(p), "other")
	if err := os.WriteFile(other, bytes.Repeat([]byte("x"), 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, p); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 16)
	if _, err := f.Read(b); err != nil || string(b) != "0123456789abcdef" {
		t.Fatalf("the handle reads %q (%v) after the path was swapped", b, err)
	}
}

func TestCheckPrivateDir(t *testing.T) {
	base := t.TempDir()
	mk := func(parent, name string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(parent, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := mk(base, "ok", 0o755)
	if err := CheckPrivateDir(ok); err != nil {
		t.Errorf("a 0755 directory of the user was refused: %v", err)
	}
	for _, mode := range []os.FileMode{0o777, 0o775, 0o757, 0o722, 0o720, 0o702} {
		d := mk(base, "w"+mode.String(), mode)
		if err := CheckPrivateDir(d); err == nil {
			t.Errorf("a directory with mode %04o was accepted", mode)
		}
	}
	// A group, even the user's own or root's, can have other members and there is no
	// list of trusted writers, so group write is refused like world write.
	own := mk(base, "own-group", 0o775)
	if err := CheckPrivateDir(own); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Errorf("a 0775 directory of the user's own group was accepted: %v", err)
	}
	if err := CheckPrivateDir(mk(base, "closed", 0o750)); err != nil {
		t.Errorf("a 0750 directory has no writer but its owner and was refused: %v", err)
	}
	// An ancestor others can write.
	open := mk(base, "open", 0o777)
	inner := mk(open, "inner", 0o700)
	if err := CheckPrivateDir(inner); err == nil {
		t.Error("a directory below a world-writable one was accepted")
	}
	// Sticky above is fine (like /tmp), sticky on the directory itself is not.
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(inner); err != nil {
		t.Errorf("a directory below a sticky world-writable one was refused: %v", err)
	}
	if err := CheckPrivateDir(open); err == nil {
		t.Error("a sticky world-writable directory itself was accepted")
	}
	// An ancestor of another owner can rename what is below it.
	old := euid
	euid = func() int { return os.Geteuid() + 4242 }
	err := CheckPrivateDir(ok)
	euid = old
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Errorf("a directory of another owner: %v", err)
	}
	// A symlink on the way is checked as well as its target.
	link := filepath.Join(base, "link")
	if err := os.Symlink(open, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(filepath.Join(link, "inner")); err != nil {
		t.Errorf("an own symlink to a trusted chain was refused: %v", err)
	}
	if err := CheckPrivateDir(filepath.Join(base, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing directory: %v", err)
	}
}

// Every hop of a symlink chain is checked: safe/hub -> shared/hop -> safe/data with
// shared writable by anyone lets another user replace the hop and redirect the path,
// although the start (safe/hub) and the end (safe/data) are safe.
func TestCheckPrivateDirChecksEveryHopOfASymlinkChain(t *testing.T) {
	base := t.TempDir()
	mkdir := func(p string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	safe := mkdir(filepath.Join(base, "safe"), 0o755)
	data := mkdir(filepath.Join(safe, "data"), 0o700)
	shared := mkdir(filepath.Join(base, "shared"), 0o777)
	if err := os.Symlink(data, filepath.Join(shared, "hop")); err != nil {
		t.Fatal(err)
	}
	hub := filepath.Join(safe, "hub")
	if err := os.Symlink(filepath.Join(shared, "hop"), hub); err != nil {
		t.Fatal(err)
	}
	err := CheckPrivateDir(hub)
	if err == nil {
		t.Fatal("a chain through a hop in a world-writable directory was accepted")
	}
	if !strings.Contains(err.Error(), shared) {
		t.Errorf("the error does not name the unsafe directory %s: %v", shared, err)
	}
	// The same through a path below the link, and through OpenPrivate's directory.
	if err := CheckPrivateDir(filepath.Join(hub, "x")); err == nil {
		if err2 := os.Mkdir(filepath.Join(data, "x"), 0o700); err2 != nil {
			t.Fatal(err2)
		}
		if err := CheckPrivateDir(filepath.Join(hub, "x")); err == nil {
			t.Error("a directory below the chain was accepted")
		}
	}
	// With the hop's directory closed the same chain is fine, and so is a relative link.
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(hub); err != nil {
		t.Errorf("a chain whose hops are all in trusted directories was refused: %v", err)
	}
	if err := os.Symlink("data", filepath.Join(safe, "rel")); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(filepath.Join(safe, "rel")); err != nil {
		t.Errorf("a relative link in a trusted directory was refused: %v", err)
	}
	// A link whose owner is not trusted is refused.
	old := euid
	euid = func() int { return os.Geteuid() + 4242 }
	err = CheckPrivateDir(filepath.Join(safe, "rel"))
	euid = old
	if err == nil {
		t.Error("a chain owned by another user was accepted")
	}
	// A loop is an error, not a hang.
	l1, l2 := filepath.Join(safe, "l1"), filepath.Join(safe, "l2")
	if err := os.Symlink(l2, l1); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l1, l2); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(l1); err == nil {
		t.Error("a symlink loop was accepted")
	}
}
