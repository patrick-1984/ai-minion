package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHubOIDCSettingsDeclared(t *testing.T) {
	want := map[string]string{
		KeyOIDCName:         "MINION_OIDC_NAME",
		KeyOIDCIssuer:       "MINION_OIDC_ISSUER",
		KeyOIDCClientID:     "MINION_OIDC_CLIENT_ID",
		KeyOIDCClientSecret: "MINION_OIDC_CLIENT_SECRET",
		KeyOIDCRoleClaim:    "MINION_OIDC_ROLE_CLAIM",
		KeyOIDCAdminValues:  "MINION_OIDC_ADMIN_VALUES",
		KeyOIDCUserValues:   "MINION_OIDC_USER_VALUES",
		KeyOIDCScopes:       "MINION_OIDC_SCOPES",
		KeyOIDCTimeout:      "MINION_OIDC_TIMEOUT",
		KeyOIDCTLSInsecure:  "MINION_OIDC_TLS_INSECURE_SKIP_VERIFY",
	}
	for key, env := range want {
		s, ok := Default.Lookup(key)
		if !ok {
			t.Fatalf("setting %s not declared", key)
		}
		if s.Scope != ScopeHub || s.EnvName() != env || s.HasFlag() {
			t.Errorf("%s: scope %s env %s flag %v", key, s.Scope, s.EnvName(), s.HasFlag())
		}
	}
	secret, _ := Default.Lookup(KeyOIDCClientSecret)
	if !secret.Secret || secret.PlainEnvAllowed || secret.FileEnvName() != "MINION_OIDC_CLIENT_SECRET_FILE" {
		t.Errorf("client secret: secret %v plain %v file %s", secret.Secret, secret.PlainEnvAllowed, secret.FileEnvName())
	}
	if EnvOIDCCAFile != "MINION_OIDC_CA_FILE" {
		t.Errorf("CA env = %s", EnvOIDCCAFile)
	}
}

func TestResolveHubOIDCOff(t *testing.T) {
	h, err := ResolveHubOIDC(hubEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if h.Configured() || h.Name != "SSO" || h.Timeout != 10*time.Second || h.InsecureSkipVerify ||
		!slices.Equal(h.Scopes, []string{"openid", "profile", "email"}) {
		t.Fatalf("defaults: %+v", h)
	}
}

func TestResolveHubOIDCFromEnvironment(t *testing.T) {
	r := hubEnv(map[string]string{
		"MINION_OIDC_ISSUER":                   "https://idp.example",
		"MINION_OIDC_CLIENT_ID":                "minion",
		"MINION_OIDC_CLIENT_SECRET_FILE":       "/run/secrets/oidc",
		"MINION_OIDC_ROLE_CLAIM":               " groups ",
		"MINION_OIDC_ADMIN_VALUES":             "admins, root ,,",
		"MINION_OIDC_USER_VALUES":              "users",
		"MINION_OIDC_CA_FILE":                  "/etc/ca.pem",
		"MINION_OIDC_TLS_INSECURE_SKIP_VERIFY": "true",
		"MINION_OIDC_NAME":                     "Acme",
		"MINION_OIDC_TIMEOUT":                  "3s",
		"MINION_OIDC_SCOPES":                   "openid groups",
	})
	r.ReadFile = func(path string) ([]byte, error) {
		if path != "/run/secrets/oidc" {
			t.Errorf("read %q", path)
		}
		return []byte("the-secret\n"), nil
	}
	h, err := ResolveHubOIDC(r)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Configured() || h.Issuer != "https://idp.example" || h.ClientID != "minion" || h.ClientSecret != "the-secret" ||
		h.RoleClaim != "groups" || !slices.Equal(h.AdminValues, []string{"admins", "root"}) ||
		!slices.Equal(h.UserValues, []string{"users"}) || h.CAFile != "/etc/ca.pem" || !h.InsecureSkipVerify ||
		h.Name != "Acme" || h.Timeout != 3*time.Second || !slices.Equal(h.Scopes, []string{"openid", "groups"}) {
		t.Fatalf("got %+v", h)
	}
}

func TestResolveHubOIDCRefusesPlainSecretAndBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"plain secret in the environment": {"MINION_OIDC_CLIENT_SECRET": "x"},
		"issuer without client id":        {"MINION_OIDC_ISSUER": "https://idp.example"},
		"bad insecure value":              {"MINION_OIDC_TLS_INSECURE_SKIP_VERIFY": "yes"},
		"bad timeout":                     {"MINION_OIDC_TIMEOUT": "soon"},
		"timeout too small":               {"MINION_OIDC_TIMEOUT": "10ms"},
		"timeout too large":               {"MINION_OIDC_TIMEOUT": "1h"},
	}
	for name, env := range cases {
		if _, err := ResolveHubOIDC(hubEnv(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := ResolveHubOIDC(hubEnv(map[string]string{"MINION_OIDC_CLIENT_SECRET": "leaky-value"}))
	if err == nil || strings.Contains(err.Error(), "leaky-value") {
		t.Errorf("error leaks the secret or is missing: %v", err)
	}
	if _, err := ResolveHubOIDC(&Resolver{Role: ScopeWorker}); err == nil {
		t.Error("a worker resolver was accepted")
	}
}

type mapPanel map[string]string

func (m mapPanel) Lookup(key string) (string, bool, error) { v, ok := m[key]; return v, ok, nil }

func TestResolveHubOIDCFromPanelAndEnvironmentWins(t *testing.T) {
	r := hubEnv(map[string]string{"MINION_OIDC_ISSUER": "https://env.example"})
	r.Panel = mapPanel{
		KeyOIDCIssuer: "https://panel.example", KeyOIDCClientID: "panel-client", KeyOIDCCAFile: "/panel/ca.pem",
		KeyOIDCTLSInsecure: "true", KeyOIDCClientSecret: "panel-secret",
	}
	h, err := ResolveHubOIDC(r)
	if err != nil {
		t.Fatal(err)
	}
	if h.Issuer != "https://env.example" || h.ClientID != "panel-client" || h.CAFile != "/panel/ca.pem" ||
		!h.InsecureSkipVerify || h.ClientSecret != "panel-secret" {
		t.Fatalf("got %+v", h)
	}
	r2 := hubEnv(map[string]string{"MINION_OIDC_CA_FILE": "/env/ca.pem"})
	r2.Panel = mapPanel{KeyOIDCCAFile: "/panel/ca.pem"}
	if h, _ = ResolveHubOIDC(r2); h.CAFile != "/env/ca.pem" {
		t.Errorf("CA file = %q, the environment must win", h.CAFile)
	}
}

func TestSplitList(t *testing.T) {
	if got := SplitList(" a, b ,,c ", ","); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %v", got)
	}
	if got := SplitList("", ","); got != nil {
		t.Errorf("got %v", got)
	}
}
