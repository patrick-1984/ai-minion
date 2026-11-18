package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/version"
)

func sumOf(b string) string {
	s := sha256.Sum256([]byte(b))
	return hex.EncodeToString(s[:])
}

func TestCopyServiceBinaryChecksTheVerifiedDigest(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "cache-minion")
	dst := filepath.Join(dir, "minion.exe")
	if err := os.WriteFile(src, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A source that was changed after it was verified is not installed.
	err := copyServiceBinary(src, dst, sumOf("something else"))
	if !errors.Is(err, ErrBinaryDigest) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Errorf("dst = %q after a refused copy", b)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".minion-install-") {
			t.Errorf("leftover: %s", e.Name())
		}
	}
	if err := copyServiceBinary(src, dst, sumOf("release")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "release" {
		t.Errorf("dst = %q", b)
	}
	// Nothing is left beside it but the update lock.
	ents, _ = os.ReadDir(dir)
	for _, e := range ents {
		if n := e.Name(); n != "cache-minion" && n != "minion.exe" && n != "minion.exe.update-lock" {
			t.Errorf("left behind: %s", n)
		}
	}
	// Without a digest (an explicitly unverified install) it copies as is.
	if err := copyServiceBinary(src, filepath.Join(dir, "other.exe"), ""); err != nil {
		t.Fatal(err)
	}
	if err := copyServiceBinary(dir, dst, ""); err == nil {
		t.Error("a directory was copied")
	}
}

// The remote update keeps the previous release as minion.exe.old for its
// crash rollback; installing the service binary must not touch it.
func TestCopyServiceBinaryKeepsTheRollbackBinaryOfTheRemoteUpdate(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "minion.exe")
	for name, content := range map[string]string{"minion.exe": "current", "minion.exe.old": "previous", "src": "release"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyServiceBinary(filepath.Join(dir, "src"), dst, sumOf("release")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst + ".old"); string(b) != "previous" {
		t.Errorf("minion.exe.old = %q", b)
	}
	if b, _ := os.ReadFile(dst); string(b) != "release" {
		t.Errorf("minion.exe = %q", b)
	}
}

// The update lock is the one "minion update" takes: an update in progress
// blocks the install, which then changes nothing.
func TestCopyServiceBinaryWaitsForTheUpdateLock(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "minion.exe")
	if err := os.WriteFile(dst, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := version.LockBinary(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := copyServiceBinary(src, dst, sumOf("release")); err == nil {
		t.Error("installed while an update held the lock")
	}
	if b, _ := os.ReadFile(dst); string(b) != "current" {
		t.Errorf("minion.exe = %q", b)
	}
}
