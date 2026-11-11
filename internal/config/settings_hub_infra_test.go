package config

import (
	"strings"
	"testing"
	"time"
)

func TestHubInfraSettingsDeclared(t *testing.T) {
	s, ok := Default.Lookup(KeyInfraDefaultOwner)
	if !ok {
		t.Fatalf("setting %s not declared", KeyInfraDefaultOwner)
	}
	if s.Scope != ScopeHub || s.EnvName() != "MINION_INFRA_DEFAULT_OWNER" || s.HasFlag() || s.Secret || s.Default != "" {
		t.Errorf("scope %s env %s flag %v secret %v default %q", s.Scope, s.EnvName(), s.HasFlag(), s.Secret, s.Default)
	}
	// The key file path is read from MINION_INFRA_INVITE_KEY_FILE directly
	// because the registry reserves the _FILE suffix for secrets.
	if EnvInfraInviteKeyFile != "MINION_INFRA_INVITE_KEY_FILE" || KeyInfraInviteKeyFile != "infra_invite_key_file" {
		t.Errorf("names: %s %s", EnvInfraInviteKeyFile, KeyInfraInviteKeyFile)
	}
	if _, ok := Default.Lookup(KeyInfraInviteKeyFile); ok {
		t.Errorf("%s must not be declared in the registry", KeyInfraInviteKeyFile)
	}
}

func TestResolveHubInfraDefaults(t *testing.T) {
	hi, err := ResolveHubInfra(hubEnv(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (HubInfra{InviteKeyLifetime: 30 * 24 * time.Hour}); hi != want {
		t.Fatalf("got %+v, want %+v: no key file, the seed admin owns, 30 days of validity", hi, want)
	}
}

func TestResolveHubInfraKeyDays(t *testing.T) {
	for in, want := range map[string]time.Duration{"7": 7 * 24 * time.Hour, "0": 0, " 365 ": 365 * 24 * time.Hour} {
		hi, err := ResolveHubInfra(hubEnv(map[string]string{"MINION_INFRA_INVITE_KEY_DAYS": in}))
		if err != nil || hi.InviteKeyLifetime != want {
			t.Errorf("days %q: %+v err=%v", in, hi, err)
		}
	}
	for _, bad := range []string{"-1", "abc", "3651", "1.5", ""} {
		if bad == "" {
			continue
		}
		if _, err := ResolveHubInfra(hubEnv(map[string]string{"MINION_INFRA_INVITE_KEY_DAYS": bad})); err == nil {
			t.Errorf("days %q was accepted", bad)
		}
	}
}

func TestResolveHubInfraValues(t *testing.T) {
	hi, err := ResolveHubInfra(hubEnv(map[string]string{
		"MINION_INFRA_INVITE_KEY_FILE": " /run/secrets/infra-key ",
		"MINION_INFRA_DEFAULT_OWNER":   " ops ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if hi.InviteKeyFile != "/run/secrets/infra-key" || hi.DefaultOwner != "ops" {
		t.Fatalf("got %+v", hi)
	}
}

func TestResolveHubInfraPanelAndEnvPrecedence(t *testing.T) {
	r := hubEnv(map[string]string{})
	r.Panel = MapSource{KeyInfraInviteKeyFile: "/panel/key", KeyInfraDefaultOwner: "panel-owner"}
	hi, err := ResolveHubInfra(r)
	if err != nil || hi.InviteKeyFile != "/panel/key" || hi.DefaultOwner != "panel-owner" {
		t.Fatalf("panel values: %+v, %v", hi, err)
	}
	r = hubEnv(map[string]string{"MINION_INFRA_INVITE_KEY_FILE": "/env/key", "MINION_INFRA_DEFAULT_OWNER": "env-owner"})
	r.Panel = MapSource{KeyInfraInviteKeyFile: "/panel/key", KeyInfraDefaultOwner: "panel-owner"}
	if hi, err = ResolveHubInfra(r); err != nil || hi.InviteKeyFile != "/env/key" || hi.DefaultOwner != "env-owner" {
		t.Fatalf("env over panel: %+v, %v", hi, err)
	}
}

func TestResolveHubInfraNeedsAHubResolver(t *testing.T) {
	_, err := ResolveHubInfra(&Resolver{Role: ScopeWorker})
	if err == nil || !strings.Contains(err.Error(), "hub") {
		t.Errorf("worker role: %v", err)
	}
}
