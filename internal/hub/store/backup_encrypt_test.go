package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func encryptedBackup(t *testing.T, dir, pass string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "enc.age")
	if _, err := Backup(context.Background(), dir, out, "dev", WithBackupPassphrase(pass)); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEncryptedBackupRoundTripDecryptsTheOIDCSecret(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	out := encryptedBackup(t, dir, "correct horse battery staple")

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), ageMagic) {
		t.Fatalf("archive does not start with the age header: %q", raw[:24])
	}
	for _, secret := range []string{"minion-ed25519-seed", "TLSKEY", "SQLite format", "manifest.json"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("the encrypted archive contains %q in clear", secret)
		}
	}
	if st, err := os.Stat(out); err != nil || (st.Mode().Perm()&0o077 != 0 && !isWindows()) {
		t.Errorf("archive mode: %v %v", st, err)
	}

	target := filepath.Join(t.TempDir(), "new", "hub")
	res, err := Restore(ctx, out, target, false, WithRestorePassphrase("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Encrypted || strings.Join(res.Files, ",") != "data.key,identity.key,tls/cert.pem,tls/key.pem,minion.db" {
		t.Fatalf("result = %+v", res)
	}
	for _, name := range []string{"data.key", "identity.key", "tls/cert.pem", "tls/key.pem"} {
		if readFile(t, filepath.Join(target, name)) != readFile(t, filepath.Join(dir, name)) {
			t.Errorf("%s differs after restore", name)
		}
	}
	s, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec, ok, err := s.GetSetting(ctx, "oidc_client_secret")
	if err != nil || !ok || rec.Value != "s3cret-value" {
		t.Fatalf("sealed setting after restore: %+v ok=%v err=%v", rec, ok, err)
	}
}

func TestRestoreWithAWrongPassphraseTouchesNothing(t *testing.T) {
	ctx := context.Background()
	out := encryptedBackup(t, newHubDir(t), "right")

	fresh := filepath.Join(t.TempDir(), "never-created")
	_, err := Restore(ctx, out, fresh, false, WithRestorePassphrase("wrong"))
	if !errors.Is(err, ErrWrongPassphrase) || !errors.Is(err, ErrBackup) {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(fresh); !os.IsNotExist(serr) {
		t.Fatalf("the destination was created: %v", serr)
	}

	// An existing Hub is left byte for byte as it was, even with --force.
	existing := markedHubDir(t, "old")
	before := readFile(t, filepath.Join(existing, DataKeyFile))
	if _, err := Restore(ctx, out, existing, true, WithRestorePassphrase("wrong")); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("force restore: %v", err)
	}
	if readFile(t, filepath.Join(existing, DataKeyFile)) != before {
		t.Fatal("data.key changed")
	}
	if left, _ := filepath.Glob(filepath.Join(existing, ".restore-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
}

func TestRestoreOfAnEncryptedArchiveNeedsAPassphrase(t *testing.T) {
	out := encryptedBackup(t, newHubDir(t), "pw")
	target := filepath.Join(t.TempDir(), "t")
	_, err := Restore(context.Background(), out, target, false)
	if !errors.Is(err, ErrPassphraseRequired) || !errors.Is(err, ErrBackup) {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(target); !os.IsNotExist(serr) {
		t.Fatal("the destination was created")
	}
}

func TestRestoreRefusesATruncatedOrTamperedEncryptedArchive(t *testing.T) {
	out := encryptedBackup(t, newHubDir(t), "pw")
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)-40] ^= 0xff
	for name, body := range map[string][]byte{
		"truncated": raw[:len(raw)-100],
		"tampered":  flipped,
	} {
		p := filepath.Join(t.TempDir(), name+".age")
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "t")
		if _, err := Restore(context.Background(), p, target, false, WithRestorePassphrase("pw")); !errors.Is(err, ErrBackup) {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, serr := os.Stat(target); !os.IsNotExist(serr) {
			t.Errorf("%s: the destination was created", name)
		}
	}
}

func TestLegacyPlainArchiveStillRestoresAndIgnoresAPassphrase(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	out := filepath.Join(t.TempDir(), "plain.tar.gz")
	if _, err := Backup(ctx, dir, out, "dev"); err != nil {
		t.Fatal(err)
	}
	for _, opts := range [][]Option{nil, {WithRestorePassphrase("unused")}} {
		target := filepath.Join(t.TempDir(), "hub")
		res, err := Restore(ctx, out, target, false, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if res.Encrypted {
			t.Fatal("a plain archive was reported as encrypted")
		}
		if readFile(t, filepath.Join(target, IdentityKeyFile)) != readFile(t, filepath.Join(dir, IdentityKeyFile)) {
			t.Fatal("identity.key differs")
		}
	}
}

func TestBackupRefusesAnEmptyPassphraseAndLeavesNoFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "x.age")
	if _, err := Backup(context.Background(), newHubDir(t), out, "dev", WithBackupPassphrase("")); !errors.Is(err, ErrBackup) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a file was left behind")
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }
