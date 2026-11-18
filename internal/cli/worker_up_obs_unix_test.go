//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// worker up checks private files with the daemon's rules: a link to a valid
// 0600 header file is refused before anything is saved.
func TestWorkerUpRefusesALinkedHeaderFile(t *testing.T) {
	f := newUpFixture(t, false)
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("A: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := f.run("--otel", "on", "--otel-endpoint", "https://c.example", "--otel-headers-path", link)
	if err == nil || !strings.Contains(err.Error(), "otel_headers_path") || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("err = %v", err)
	}
}

// A relative path already in the config file is refused; a padded flag
// value is trimmed and saved absolute, as it was checked.
func TestWorkerUpServicePaths(t *testing.T) {
	f := newUpFixture(t, false)
	if err := os.MkdirAll(f.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(filepath.Join(f.stateDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("otel", "on")
	cfg.Set("otel_endpoint", "https://c.example")
	cfg.Set("otel_headers_path", "hdr")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(); err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Fatalf("err = %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hdr"), []byte("A: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if _, err := f.run("--otel-headers-path", "  hdr "); err != nil {
		t.Fatalf("%v\n%s", err, f.stderr)
	}
	if got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json")); got["otel_headers_path"] != filepath.Join(dir, "hdr") {
		t.Fatalf("saved %q", got["otel_headers_path"])
	}
}

// For an install by root for another account, the file must belong to that
// account and must not be a link.
func TestServiceOwnedPrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceOwnedPrivate(p, os.Getuid()); err != nil {
		t.Fatalf("own file: %v", err)
	}
	if _, err := serviceOwnedPrivate(p, os.Getuid()+1); err == nil {
		t.Fatal("a file of another account was accepted")
	}
	l := filepath.Join(dir, "l")
	_ = os.Symlink(p, l)
	if _, err := serviceOwnedPrivate(l, os.Getuid()); err == nil {
		t.Fatal("a link was accepted")
	}
	_ = os.Chmod(p, 0o640)
	if _, err := serviceOwnedPrivate(p, os.Getuid()); err == nil {
		t.Fatal("a group-readable file was accepted")
	}
}
