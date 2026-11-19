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

func writeAll(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readOr(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func TestCopyServiceBinaryChecksTheVerifiedDigest(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{"src": "release", "minion.exe": "old"})
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "minion.exe")
	// A source that was changed after it was verified is not installed.
	if _, err := copyServiceBinary(src, dst, sumOf("something else")); !errors.Is(err, ErrBinaryDigest) {
		t.Fatalf("err = %v", err)
	}
	if got := readOr(dst); got != "old" {
		t.Errorf("dst = %q after a refused copy", got)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".minion-install-") {
			t.Errorf("leftover: %s", e.Name())
		}
	}
	prev, err := copyServiceBinary(src, dst, sumOf("release"))
	if err != nil {
		t.Fatal(err)
	}
	if readOr(dst) != "release" || prev != dst+".previous" || readOr(prev) != "old" {
		t.Errorf("dst %q, previous %s = %q", readOr(dst), prev, readOr(prev))
	}
	if _, err := copyServiceBinary(dir, dst, ""); err == nil {
		t.Error("a directory was copied")
	}
}

func installedAt(versions map[string]string) func(string) (version.SemVer, bool) {
	return func(p string) (version.SemVer, bool) {
		v, ok := versions[readOr(p)]
		if !ok {
			return version.SemVer{}, false
		}
		s, err := version.ParseVersion(v)
		return s, err == nil
	}
}

// The remote update keeps the previous release as minion.exe.old for its
// crash rollback; installing the service binary must not touch it. The
// replaced binary goes once the new one is active.
func TestReplaceAndActivateKeepsTheRollbackBinaryAndRetiresThePrevious(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{"minion.exe": "current", "minion.exe.old": "previous", "src": "release"})
	dst := filepath.Join(dir, "minion.exe")
	var active string
	err := replaceAndActivate(dst, BinaryInstall{Src: filepath.Join(dir, "src"), SHA256: sumOf("release")}, nil,
		func(p string) error { active = readOr(p); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if active != "release" || readOr(dst) != "release" || readOr(dst+".old") != "previous" {
		t.Errorf("active %q, dst %q, old %q", active, readOr(dst), readOr(dst+".old"))
	}
	if _, err := os.Stat(dst + ".previous"); !os.IsNotExist(err) {
		t.Errorf("the replaced binary stays: %v", err)
	}
}

// A failed activation puts the previous binary back.
func TestReplaceAndActivatePutsThePreviousBinaryBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{"minion.exe": "current", "src": "release"})
	dst := filepath.Join(dir, "minion.exe")
	boom := errors.New("the service did not start")
	err := replaceAndActivate(dst, BinaryInstall{Src: filepath.Join(dir, "src"), SHA256: sumOf("release")}, nil,
		func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if readOr(dst) != "current" {
		t.Errorf("dst = %q", readOr(dst))
	}
	if _, err := os.Stat(dst + ".previous"); !os.IsNotExist(err) {
		t.Errorf("previous left: %v", err)
	}
}

// The Hub may have updated the service past the CLI that runs "worker up":
// that is not undone without an explicit flag.
func TestReplaceAndActivateRefusesADowngrade(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{"minion.exe": "hub-updated", "src": "release"})
	dst := filepath.Join(dir, "minion.exe")
	installed := installedAt(map[string]string{"hub-updated": "v1.3.0", "release": "v1.2.0"})
	bi := BinaryInstall{Src: filepath.Join(dir, "src"), SHA256: sumOf("release"), Version: "v1.2.0"}
	activated := false
	err := replaceAndActivate(dst, bi, installed, func(string) error { activated = true; return nil })
	if !errors.Is(err, ErrServiceDowngrade) || !strings.Contains(err.Error(), "--allow-service-downgrade") {
		t.Fatalf("err = %v", err)
	}
	if activated || readOr(dst) != "hub-updated" {
		t.Errorf("activated %v, dst %q", activated, readOr(dst))
	}
	bi.AllowDowngrade = true
	if err := replaceAndActivate(dst, bi, installed, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if readOr(dst) != "release" {
		t.Errorf("dst = %q", readOr(dst))
	}
	// The same or a newer release needs no flag.
	writeAll(t, dir, map[string]string{"src": "newer"})
	installed = installedAt(map[string]string{"release": "v1.2.0", "newer": "v1.4.0"})
	if err := replaceAndActivate(dst, BinaryInstall{Src: filepath.Join(dir, "src"), SHA256: sumOf("newer"), Version: "v1.4.0"}, installed,
		func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// The update lock is the one "minion update" and the remote update take: it
// is held from the copy through the activation, and an update in progress
// blocks the install, which then changes nothing.
func TestReplaceAndActivateHoldsTheUpdateLock(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{"minion.exe": "current", "src": "release"})
	dst := filepath.Join(dir, "minion.exe")
	bi := BinaryInstall{Src: filepath.Join(dir, "src"), SHA256: sumOf("release")}
	err := replaceAndActivate(dst, bi, nil, func(string) error {
		if _, err := version.LockBinary(dst); err == nil {
			t.Error("the update lock is free during the activation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	release, err := version.LockBinary(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	writeAll(t, dir, map[string]string{"src": "other"})
	if err := replaceAndActivate(dst, BinaryInstall{Src: filepath.Join(dir, "src"), SHA256: sumOf("other")}, nil,
		func(string) error { return nil }); err == nil {
		t.Error("installed while an update held the lock")
	}
	if readOr(dst) != "release" {
		t.Errorf("minion.exe = %q", readOr(dst))
	}
}
