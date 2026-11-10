package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openSettingsStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSettingsTableComesFromEmbeddedMigration(t *testing.T) {
	s := openSettingsStore(t)
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = 2 AND name = 'settings'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migration 0002_settings not recorded (count %d)", n)
	}
}

func TestPlainSettingRoundTrip(t *testing.T) {
	s := openSettingsStore(t)
	ctx := context.Background()

	if _, ok, err := s.GetSetting(ctx, "log_level"); err != nil || ok {
		t.Fatalf("missing key: ok=%v err=%v", ok, err)
	}
	if err := s.PutSetting(ctx, "log_level", "debug", false, "admin-1"); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := s.GetSetting(ctx, "log_level")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if rec.Value != "debug" || rec.Secret || rec.UpdatedBy != "admin-1" || rec.UpdatedAt.IsZero() {
		t.Fatalf("record = %+v", rec)
	}

	// Replace, then list.
	if err := s.PutSetting(ctx, "log_level", "warn", false, "admin-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "a_first", "x", false, ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Key != "a_first" || list[1].Key != "log_level" || list[1].Value != "warn" || list[1].UpdatedBy != "admin-2" {
		t.Fatalf("list = %+v", list)
	}
}

func TestSecretSettingIsEncryptedAtRest(t *testing.T) {
	s := openSettingsStore(t)
	ctx := context.Background()
	const secret = "s3cr3t-value-that-must-not-leak"

	if err := s.PutSetting(ctx, "oidc_client_secret", secret, true, "admin"); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := s.GetSetting(ctx, "oidc_client_secret")
	if err != nil || !ok || rec.Value != secret || !rec.Secret {
		t.Fatalf("get = %+v ok=%v err=%v", rec, ok, err)
	}

	var text *string
	var sealed []byte
	if err := s.DB().QueryRow(`SELECT value, value_enc FROM settings WHERE key = 'oidc_client_secret'`).Scan(&text, &sealed); err != nil {
		t.Fatal(err)
	}
	if text != nil {
		t.Fatalf("plain column holds %q", *text)
	}
	if len(sealed) == 0 || bytes.Contains(sealed, []byte(secret)) {
		t.Fatalf("value_enc not sealed: %x", sealed)
	}

	// The database file (and its WAL) never contains the plaintext.
	for _, f := range []string{DBFile, DBFile + "-wal"} {
		data, err := os.ReadFile(filepath.Join(s.Dir(), f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("%s contains the plaintext secret", f)
		}
	}
}

func TestSecretSettingIsBoundToItsKey(t *testing.T) {
	s := openSettingsStore(t)
	ctx := context.Background()
	if err := s.PutSetting(ctx, "key_a", "value-a", true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "key_b", "value-b", true, ""); err != nil {
		t.Fatal(err)
	}
	// Copy the sealed bytes of key_a onto key_b: it must not open.
	if _, err := s.DB().Exec(`UPDATE settings SET value_enc = (SELECT value_enc FROM settings WHERE key = 'key_a') WHERE key = 'key_b'`); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.GetSetting(ctx, "key_b")
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("copied ciphertext opened or wrong error: %v", err)
	}
	if strings.Contains(err.Error(), "value-a") {
		t.Fatalf("error leaks the value: %v", err)
	}
	if _, err := s.ListSettings(ctx); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("list should fail with ErrDecrypt, got %v", err)
	}
}

func TestPutSettingSwitchesBetweenPlainAndSecret(t *testing.T) {
	s := openSettingsStore(t)
	ctx := context.Background()
	if err := s.PutSetting(ctx, "k", "plain", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "k", "now-secret", true, ""); err != nil {
		t.Fatal(err)
	}
	rec, _, err := s.GetSetting(ctx, "k")
	if err != nil || rec.Value != "now-secret" || !rec.Secret {
		t.Fatalf("rec = %+v err=%v", rec, err)
	}
	var text *string
	if err := s.DB().QueryRow(`SELECT value FROM settings WHERE key = 'k'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != nil {
		t.Fatalf("plain column still set: %q", *text)
	}
}

func TestEmptyValuesAreStored(t *testing.T) {
	s := openSettingsStore(t)
	ctx := context.Background()
	for _, secret := range []bool{false, true} {
		if err := s.PutSetting(ctx, "empty", "", secret, ""); err != nil {
			t.Fatalf("secret=%v: %v", secret, err)
		}
		rec, ok, err := s.GetSetting(ctx, "empty")
		if err != nil || !ok || rec.Value != "" || rec.Secret != secret {
			t.Fatalf("secret=%v: rec=%+v ok=%v err=%v", secret, rec, ok, err)
		}
	}
}

func TestDeleteSetting(t *testing.T) {
	s := openSettingsStore(t)
	ctx := context.Background()
	if existed, err := s.DeleteSetting(ctx, "nope"); err != nil || existed {
		t.Fatalf("delete missing: existed=%v err=%v", existed, err)
	}
	if err := s.PutSetting(ctx, "k", "v", false, ""); err != nil {
		t.Fatal(err)
	}
	if existed, err := s.DeleteSetting(ctx, "k"); err != nil || !existed {
		t.Fatalf("delete: existed=%v err=%v", existed, err)
	}
	if _, ok, _ := s.GetSetting(ctx, "k"); ok {
		t.Fatal("still present after delete")
	}
}

func TestPutSettingRejectsEmptyKey(t *testing.T) {
	s := openSettingsStore(t)
	if err := s.PutSetting(context.Background(), "", "v", false, ""); err == nil {
		t.Fatal("empty key accepted")
	}
}

func TestSettingsSurviveReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.PutSetting(ctx, "k", "secret-v", true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rec, ok, err := s2.GetSetting(ctx, "k")
	if err != nil || !ok || rec.Value != "secret-v" {
		t.Fatalf("after reopen: %+v ok=%v err=%v", rec, ok, err)
	}
}

func TestSettingRecordRedactsASecretWhenPrinted(t *testing.T) {
	const plain = "hunter2-very-secret"
	rec := SettingRecord{Key: "api_token", Value: plain, Secret: true, UpdatedBy: "admin-1"}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("setting", "rec", rec)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("setting", slog.Any("rec", rec))
	for name, out := range map[string]string{
		"%v":   fmt.Sprintf("%v", rec),
		"%+v":  fmt.Sprintf("%+v", rec),
		"%#v":  fmt.Sprintf("%#v", rec),
		"%v *": fmt.Sprintf("%v", &rec),
		"slog": buf.String(),
	} {
		if strings.Contains(out, plain) {
			t.Errorf("%s leaks the secret: %s", name, out)
		}
		if !strings.Contains(out, "[redacted]") || !strings.Contains(out, "api_token") {
			t.Errorf("%s = %q, want the key and a redaction marker", name, out)
		}
	}
	// A plain value, and an empty secret, print as they are.
	if out := fmt.Sprintf("%v", SettingRecord{Key: "k", Value: "visible"}); !strings.Contains(out, "visible") {
		t.Errorf("plain value hidden: %s", out)
	}
	if out := fmt.Sprintf("%v", SettingRecord{Key: "k", Secret: true}); strings.Contains(out, "[redacted]") {
		t.Errorf("empty secret shown as redacted: %s", out)
	}
	if rec.Value != plain {
		t.Fatal("printing changed the value")
	}
}
