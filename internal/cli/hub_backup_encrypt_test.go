package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

func writePassphraseFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pass")
	if err := identity.WritePrivate(p, []byte(content)); err != nil {
		t.Fatal(err)
	}
	return p
}

func newBackupSource(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "hub")
	s, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(context.Background(), "oidc_client_secret", "s3cret", true, "t"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	return src
}

func TestHubBackupWithoutAnEncryptionChoiceIsRefused(t *testing.T) {
	src := newBackupSource(t)
	archive := filepath.Join(t.TempDir(), "a")
	err := runHubBackup(context.Background(), &bytes.Buffer{}, hubBackupDeps{}, src, archive, hubBackupOpts{})
	if err == nil || !strings.Contains(err.Error(), "--passphrase-file") || !strings.Contains(err.Error(), "--unencrypted") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(archive); serr == nil {
		t.Fatal("an archive was written")
	}
	// The same through the command line.
	var out, errb bytes.Buffer
	code := Execute(context.Background(), []string{"hub", "backup", "--data-dir", src, "--out", archive}, &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "--passphrase-file") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
	both := hubBackupOpts{passphraseFile: writePassphraseFile(t, "x"), unencrypted: true}
	if err := runHubBackup(context.Background(), &bytes.Buffer{}, hubBackupDeps{}, src, archive, both); err == nil {
		t.Fatal("both flags were accepted")
	}
}

func TestHubBackupUnencryptedWarns(t *testing.T) {
	src := newBackupSource(t)
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	var out bytes.Buffer
	if err := runHubBackup(context.Background(), &out, hubBackupDeps{}, src, archive, plainBackup); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "NOT encrypted") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestHubBackupEncryptedRestoreFlow(t *testing.T) {
	src := newBackupSource(t)
	archive := filepath.Join(t.TempDir(), "a.age")
	pf := writePassphraseFile(t, "pass phrase\r\n")
	var out bytes.Buffer
	if err := runHubBackup(context.Background(), &out, hubBackupDeps{}, src, archive, hubBackupOpts{passphraseFile: pf}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "encrypted with age") {
		t.Fatalf("output = %q", out.String())
	}
	d := hubBackupDeps{containerDir: func(string) containerKind { return notContainer }}

	// No passphrase: a clear error naming the flag, nothing created.
	dst := filepath.Join(t.TempDir(), "d")
	err := runHubRestore(context.Background(), &out, d, dst, archive, false, "", 0)
	if !errors.Is(err, store.ErrPassphraseRequired) || !strings.Contains(err.Error(), "--passphrase-file") {
		t.Fatalf("err = %v", err)
	}
	// Wrong passphrase.
	err = runHubRestore(context.Background(), &out, d, dst, archive, false, writePassphraseFile(t, "nope"), 0)
	if !errors.Is(err, store.ErrWrongPassphrase) {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Fatal("the destination was created by a failed restore")
	}
	// Right passphrase (trailing CRLF ignored), and the OIDC secret decrypts.
	if err := runHubRestore(context.Background(), &out, d, dst, archive, false, writePassphraseFile(t, "pass phrase\n"), 0); err != nil {
		t.Fatal(err)
	}
	r, err := store.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if rec, ok, err := r.GetSetting(context.Background(), "oidc_client_secret"); err != nil || !ok || rec.Value != "s3cret" {
		t.Fatalf("secret after restore: %+v %v %v", rec, ok, err)
	}
}

func TestPassphraseFileMustBePrivateAndNonEmpty(t *testing.T) {
	if _, err := readPassphraseFile(writePassphraseFile(t, "\n")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty: %v", err)
	}
	if _, err := readPassphraseFile(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	open := writePassphraseFile(t, "secret")
	if err := os.Chmod(open, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPassphraseFile(open); err == nil {
		t.Fatal("a world-readable passphrase file was accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(writePassphraseFile(t, "secret"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPassphraseFile(link); err == nil {
		t.Fatal("a symlink was accepted")
	}
	src := newBackupSource(t)
	err := runHubBackup(context.Background(), &bytes.Buffer{}, hubBackupDeps{}, src, filepath.Join(t.TempDir(), "a"), hubBackupOpts{passphraseFile: open})
	if err == nil {
		t.Fatal("backup with an exposed passphrase file succeeded")
	}
}

func TestHubRestoreMaxSizeRefusesAnOversizedArchive(t *testing.T) {
	src := newBackupSource(t)
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := runHubBackup(context.Background(), &bytes.Buffer{}, hubBackupDeps{}, src, archive, hubBackupOpts{unencrypted: true}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	err := runHubRestore(context.Background(), &bytes.Buffer{}, hubBackupDeps{}, dst, archive, false, "", 10)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Fatal("the destination was created")
	}
}
