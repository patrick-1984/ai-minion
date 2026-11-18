package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	if len(ents) != 2 {
		t.Errorf("leftovers: %v", ents)
	}
	if err := copyServiceBinary(src, dst, sumOf("release")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "release" {
		t.Errorf("dst = %q", b)
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Errorf("the old binary was kept: %v", err)
	}
	// Without a digest (an explicitly unverified install) it copies as is.
	if err := copyServiceBinary(src, filepath.Join(dir, "other.exe"), ""); err != nil {
		t.Fatal(err)
	}
	if err := copyServiceBinary(dir, dst, ""); err == nil {
		t.Error("a directory was copied")
	}
}
