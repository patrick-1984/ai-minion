//go:build !windows

package store

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An existing data key protects the sealed OIDC client secrets and refresh
// tokens. A key another local user can read, replace or reach through a link is
// not safe to use, and Open must say so instead of starting normally. These
// tests pin the finding "existing Hub encryption keys are accepted with public
// permissions": the file is opened once, without following a link, and checked
// on that handle, and the directories above it must not let another user swap it.

func distinctKey() []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = byte(0xa0 + i)
	}
	return k
}

func keyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func putKey(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, DataKeyFile)
	if err := os.WriteFile(p, distinctKey(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertNoSecret(t *testing.T, what string, text string) {
	t.Helper()
	k := distinctKey()
	for _, enc := range []string{string(k), hex.EncodeToString(k), base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k)} {
		if strings.Contains(text, enc) {
			t.Errorf("%s carries the key bytes: %q", what, text)
		}
	}
}

func TestExistingDataKeyWithGroupOrOtherAccessIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666, 0o610, 0o601, 0o777} {
		dir := keyDir(t)
		p := putKey(t, dir, mode)
		key, err := LoadOrCreateKey(p)
		if err == nil {
			t.Errorf("mode %04o: the key was accepted: %x", mode, key)
			continue
		}
		if key != nil {
			t.Errorf("mode %04o: key bytes returned with the error", mode)
		}
		msg := err.Error()
		for _, want := range []string{p, "other users", "not changed", "exposed"} {
			if !strings.Contains(msg, want) {
				t.Errorf("mode %04o: the operator guidance lacks %q: %s", mode, want, msg)
			}
		}
		assertNoSecret(t, "the error", msg)
		// It is never quietly secured and then used: the file keeps its mode.
		if fi, _ := os.Stat(p); fi.Mode().Perm() != mode {
			t.Errorf("mode %04o: the file was changed to %04o", mode, fi.Mode().Perm())
		}
	}
}

func TestOpenRefusesAnExposedDataKeyBeforeCreatingAnything(t *testing.T) {
	dir := keyDir(t)
	putKey(t, dir, 0o644)
	if _, err := Open(dir); err == nil {
		t.Fatal("Open started a Hub on a data key that other users can read")
	} else {
		assertNoSecret(t, "the Open error", err.Error())
	}
	if _, err := os.Stat(filepath.Join(dir, DBFile)); err == nil {
		t.Error("a database was created next to an unsafe key")
	}
}

func TestOwnerOnlyDataKeyIsAccepted(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		dir := keyDir(t)
		p := putKey(t, dir, mode)
		key, err := LoadOrCreateKey(p)
		if err != nil || !bytes.Equal(key, distinctKey()) {
			t.Errorf("mode %04o: %x, %v", mode, key, err)
		}
	}
}

func TestDataKeyBehindASymlinkIsRefused(t *testing.T) {
	dir := keyDir(t)
	real := filepath.Join(t.TempDir(), "elsewhere.key")
	if err := os.WriteFile(real, distinctKey(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, DataKeyFile)); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateKey(filepath.Join(dir, DataKeyFile))
	if err == nil {
		t.Fatal("a symlinked data key was accepted")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("the error does not say why: %v", err)
	}
	// A dangling link is not "no key yet": the key is not created through it.
	if err := os.Remove(real); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(filepath.Join(dir, DataKeyFile)); err == nil {
		t.Fatal("a dangling symlink was treated as a missing key")
	}
	if _, err := os.Lstat(real); err == nil {
		t.Error("a key was created through the dangling link")
	}
}

func TestDataKeyThatIsNotARegularFileIsRefusedWithoutBlocking(t *testing.T) {
	dir := keyDir(t)
	p := filepath.Join(dir, DataKeyFile)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("no fifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadOrCreateKey(p); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was accepted as the data key")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO as the data key blocks the Hub")
	}
	dir2 := keyDir(t)
	if err := os.Mkdir(filepath.Join(dir2, DataKeyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(filepath.Join(dir2, DataKeyFile)); err == nil {
		t.Fatal("a directory was accepted as the data key")
	}
}

func TestDataKeyInADirectoryOthersCanWriteIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o775, 0o757, 0o702} {
		dir := keyDir(t)
		p := putKey(t, dir, 0o600)
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadOrCreateKey(p)
		if err == nil {
			t.Errorf("dir %04o: a key in a directory others can write was accepted", mode)
			continue
		}
		if !strings.Contains(err.Error(), "replace") {
			t.Errorf("dir %04o: the error does not explain the risk: %v", mode, err)
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != mode {
			t.Errorf("dir %04o: the directory was changed to %04o", mode, fi.Mode().Perm())
		}
		_ = os.Chmod(dir, 0o700)
	}
	// An ancestor that others can write (not sticky) lets them rename the data directory.
	base := t.TempDir()
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(open, "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := putKey(t, dir, 0o600)
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(p); err == nil {
		t.Error("a key below a directory that others can write was accepted")
	}
	// A sticky directory (like /tmp) only lets an owner remove its own entries.
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(p); err != nil {
		t.Errorf("a key below a sticky world-writable directory (like /tmp) was refused: %v", err)
	}
	_ = os.Chmod(open, 0o700)
}

func TestDataKeyIsReadThroughTheCheckedHandle(t *testing.T) {
	// Opening and checking are one step on one handle: a file swapped in at the
	// path after the open cannot be the one that was read. The check function is
	// the one LoadOrCreateKey uses.
	dir := keyDir(t)
	p := putKey(t, dir, 0o600)
	f, err := openDataKey(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	evil := filepath.Join(dir, "evil")
	if err := os.WriteFile(evil, bytes.Repeat([]byte{9}, KeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(evil, p); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, KeySize)
	if _, err := f.Read(got); err != nil || !bytes.Equal(got, distinctKey()) {
		t.Fatalf("the handle no longer reads the key that was checked: %x %v", got, err)
	}
}

func TestExposedKeyStillBlocksFullOpenAfterCreateRace(t *testing.T) {
	// A key published by a concurrent starter goes through the same check.
	dir := keyDir(t)
	p := filepath.Join(dir, DataKeyFile)
	if err := os.WriteFile(p, distinctKey(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := createKeyFile(p, make([]byte, KeySize)); err == nil {
		t.Fatal("createKeyFile returned the key of an existing exposed file")
	}
}

func TestDataKeyBehindASymlinkHopInASharedDirectoryIsRefused(t *testing.T) {
	base := t.TempDir()
	safe := filepath.Join(base, "safe")
	data := filepath.Join(safe, "data")
	shared := filepath.Join(base, "shared")
	for _, d := range []string{data, shared} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chmod(safe, 0o755)
	p := putKey(t, data, 0o600)
	_ = p
	_ = os.Chmod(shared, 0o777)
	if err := os.Symlink(data, filepath.Join(shared, "hop")); err != nil {
		t.Fatal(err)
	}
	hub := filepath.Join(safe, "hub")
	if err := os.Symlink(filepath.Join(shared, "hop"), hub); err != nil {
		t.Fatal(err)
	}
	key, err := LoadOrCreateKey(filepath.Join(hub, DataKeyFile))
	if err == nil {
		t.Fatalf("a data key reached through a hop that anyone can replace was accepted: %x", key)
	}
	if !strings.Contains(err.Error(), shared) {
		t.Errorf("the error does not name the unsafe directory: %v", err)
	}
	assertNoSecret(t, "the error", err.Error())
}
