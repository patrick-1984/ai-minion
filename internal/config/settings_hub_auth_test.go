package config

import (
	"strings"
	"testing"
	"time"
)

func TestHubAuthSettingsDeclared(t *testing.T) {
	for key, env := range map[string]string{
		KeyAdminUsername: "MINION_ADMIN_USERNAME",
		KeyAdminPassword: "MINION_ADMIN_PASSWORD",
		KeySessionTTL:    "MINION_SESSION_TTL",
	} {
		s, ok := Default.Lookup(key)
		if !ok {
			t.Fatalf("setting %s not declared", key)
		}
		if s.Scope != ScopeHub || s.EnvName() != env || s.HasFlag() {
			t.Errorf("%s: scope %s env %s flag %v", key, s.Scope, s.EnvName(), s.HasFlag())
		}
	}
	pw, _ := Default.Lookup(KeyAdminPassword)
	if !pw.Secret || !pw.PlainEnvAllowed || pw.FileEnvName() != "MINION_ADMIN_PASSWORD_FILE" {
		t.Errorf("admin_password: secret %v plainEnvAllowed %v file env %s", pw.Secret, pw.PlainEnvAllowed, pw.FileEnvName())
	}
}

func TestResolveHubAuthDefaults(t *testing.T) {
	ha, err := ResolveHubAuth(hubEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := HubAuth{AdminUsername: "admin", SessionTTL: 24 * time.Hour}
	if ha != want {
		t.Fatalf("got %+v, want %+v", ha, want)
	}
}

func TestResolveHubAuthValues(t *testing.T) {
	ha, err := ResolveHubAuth(hubEnv(map[string]string{
		"MINION_ADMIN_USERNAME": " root ",
		"MINION_ADMIN_PASSWORD": "s3cret",
		"MINION_SESSION_TTL":    "90m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := HubAuth{AdminUsername: "root", AdminPassword: "s3cret", SessionTTL: 90 * time.Minute}
	if ha != want {
		t.Fatalf("got %+v, want %+v", ha, want)
	}
}

func TestResolveHubAuthPasswordFile(t *testing.T) {
	r := hubEnv(map[string]string{"MINION_ADMIN_PASSWORD_FILE": "/run/secrets/pw"})
	r.ReadFile = func(path string) ([]byte, error) {
		if path != "/run/secrets/pw" {
			t.Errorf("read %q", path)
		}
		return []byte("from-file\n"), nil
	}
	ha, err := ResolveHubAuth(r)
	if err != nil {
		t.Fatal(err)
	}
	if ha.AdminPassword != "from-file" {
		t.Fatalf("password = %q", ha.AdminPassword)
	}
}

func TestResolveHubAuthRejectsBadTTL(t *testing.T) {
	for _, v := range []string{"soon", "1m", "9000h", "-1h"} {
		_, err := ResolveHubAuth(hubEnv(map[string]string{"MINION_SESSION_TTL": v}))
		if err == nil || !strings.Contains(err.Error(), "session_ttl") {
			t.Errorf("ttl %q: err = %v", v, err)
		}
	}
}

func TestResolveHubAuthNeedsHubResolver(t *testing.T) {
	if _, err := ResolveHubAuth(&Resolver{Role: ScopeWorker}); err == nil {
		t.Fatal("a worker resolver must be refused")
	}
}

func TestResolveSessionTTLFallsBackForAnInvalidPanelValueOnly(t *testing.T) {
	r := hubEnv(nil)
	r.Panel = MapSource{KeySessionTTL: "1d"}
	got, err := ResolveSessionTTL(r)
	if err != nil || !got.Fallback || got.Value != DefaultSessionTTL || got.Source != SourcePanel {
		t.Fatalf("panel value 1d: %+v, %v", got, err)
	}
	ha, err := ResolveHubAuth(r)
	if err != nil || ha.SessionTTL != DefaultSessionTTL || ha.SessionTTLFallback != SourcePanel {
		t.Fatalf("ResolveHubAuth: %+v, %v", ha, err)
	}

	r.Panel = MapSource{KeySessionTTL: "2h"}
	if got, err := ResolveSessionTTL(r); err != nil || got.Fallback || got.Value != 2*time.Hour {
		t.Fatalf("panel value 2h: %+v, %v", got, err)
	}

	// An invalid environment value is the operator's mistake and stays an error.
	r = hubEnv(map[string]string{"MINION_SESSION_TTL": "1s"})
	r.Panel = MapSource{KeySessionTTL: "2h"}
	if _, err := ResolveSessionTTL(r); err == nil {
		t.Fatal("an invalid environment value was accepted")
	}
}
