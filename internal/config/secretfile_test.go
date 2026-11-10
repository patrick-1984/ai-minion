package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got, err := ReadSecretFile(write("a", "pw\n")); err != nil || got != "pw" {
		t.Errorf("trailing newline: %q %v", got, err)
	}
	if got, err := ReadSecretFile(write("b", "pw")); err != nil || got != "pw" {
		t.Errorf("no newline: %q %v", got, err)
	}
	if got, err := ReadSecretFile(write("c", "p w\r\n")); err != nil || got != "p w" {
		t.Errorf("crlf: %q %v", got, err)
	}
	if got, err := ReadSecretFile(write("cr", "pw\r")); err != nil || got != "pw\r" {
		t.Errorf("lone CR must stay: %q %v", got, err)
	}
	if _, err := ReadSecretFile(write("d", "")); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty: %v", err)
	}
	if _, err := ReadSecretFile(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "cannot read secret file") {
		t.Errorf("missing: %v", err)
	}
	if _, err := ReadSecretFile(""); err == nil {
		t.Error("empty path accepted")
	}
	if _, err := ReadSecretFile(dir); err == nil {
		t.Error("directory accepted")
	}
	big := write("big", strings.Repeat("x", maxSecretFileSize+1))
	if _, err := ReadSecretFile(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized: %v", err)
	}
}

func TestResolverReadsRealSecretFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(p, []byte("from-disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{
		Registry:  hubRegistry(),
		Role:      ScopeHub,
		LookupEnv: envMap(map[string]string{"MINION_OIDC_CLIENT_SECRET_FILE": p}),
	}
	got, err := r.Resolve("oidc_client_secret")
	if err != nil || got.Value != "from-disk" {
		t.Fatalf("%+v %v", got, err)
	}
}
