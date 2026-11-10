package config

import (
	"strings"
	"testing"
)

func hubEnv(m map[string]string) *Resolver {
	return &Resolver{Role: ScopeHub, LookupEnv: func(k string) (string, bool) { v, ok := m[k]; return v, ok }}
}

func TestHubServerSettingsDeclared(t *testing.T) {
	want := map[string]string{
		KeyListen:         "MINION_LISTEN",
		KeyTLS:            "MINION_TLS",
		KeyPublicURL:      "MINION_PUBLIC_URL",
		KeyTrustedProxies: "MINION_TRUSTED_PROXIES",
	}
	for key, env := range want {
		s, ok := Default.Lookup(key)
		if !ok {
			t.Fatalf("setting %s not declared", key)
		}
		if s.Scope != ScopeHub || s.EnvName() != env || s.HasFlag() || s.Secret {
			t.Errorf("%s: scope %s env %s flag %v secret %v", key, s.Scope, s.EnvName(), s.HasFlag(), s.Secret)
		}
	}
	// The certificate paths are read from MINION_TLS_*_FILE directly because
	// the registry reserves the _FILE suffix for secrets.
	for _, key := range []string{KeyTLSCertFile, KeyTLSKeyFile} {
		if _, ok := Default.Lookup(key); ok {
			t.Errorf("%s must not be declared in the registry", key)
		}
	}
}

func TestResolveHubServerDefaults(t *testing.T) {
	hs, err := ResolveHubServer(hubEnv(map[string]string{"MINION_DATA_DIR": "/data"}))
	if err != nil {
		t.Fatal(err)
	}
	want := HubServer{DataDir: "/data", Listen: ":8443", TLS: TLSAuto}
	if hs != want {
		t.Fatalf("got %+v, want %+v", hs, want)
	}
}

func TestResolveHubServerValues(t *testing.T) {
	hs, err := ResolveHubServer(hubEnv(map[string]string{
		"MINION_DATA_DIR":        "/d",
		"MINION_LISTEN":          "0.0.0.0:9000",
		"MINION_TLS":             "off",
		"MINION_PUBLIC_URL":      " https://hub.example.com ",
		"MINION_TRUSTED_PROXIES": "10.0.0.0/8",
		"MINION_TLS_CERT_FILE":   "/c.pem",
		"MINION_TLS_KEY_FILE":    "/k.pem",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if hs.Listen != "0.0.0.0:9000" || hs.TLS != TLSOff || hs.PublicURL != "https://hub.example.com" ||
		hs.TrustedProxies != "10.0.0.0/8" || hs.CertFile != "/c.pem" || hs.KeyFile != "/k.pem" {
		t.Fatalf("got %+v", hs)
	}
}

func TestResolveHubServerErrors(t *testing.T) {
	if _, err := ResolveHubServer(hubEnv(map[string]string{"MINION_DATA_DIR": "/d", "MINION_TLS": "on"})); err == nil {
		t.Error("tls=on accepted")
	}
	for _, only := range []string{"MINION_TLS_CERT_FILE", "MINION_TLS_KEY_FILE"} {
		_, err := ResolveHubServer(hubEnv(map[string]string{"MINION_DATA_DIR": "/d", only: "/x"}))
		if err == nil || !strings.Contains(err.Error(), "both") {
			t.Errorf("only %s: %v", only, err)
		}
	}
	if _, err := ResolveHubServer(&Resolver{Role: ScopeWorker}); err == nil {
		t.Error("worker role accepted")
	}
}

func TestResolveHubServerCertPathsFromPanelAndEnvPrecedence(t *testing.T) {
	panel := MapSource{KeyTLSCertFile: " /panel/c.pem ", KeyTLSKeyFile: "/panel/k.pem"}
	r := hubEnv(map[string]string{"MINION_DATA_DIR": "/d"})
	r.Panel = panel
	hs, err := ResolveHubServer(r)
	if err != nil {
		t.Fatal(err)
	}
	if hs.CertFile != "/panel/c.pem" || hs.KeyFile != "/panel/k.pem" {
		t.Fatalf("panel values were discarded: %+v", hs)
	}

	// The environment overrides the panel, per setting.
	r = hubEnv(map[string]string{"MINION_DATA_DIR": "/d", "MINION_TLS_CERT_FILE": "/env/c.pem"})
	r.Panel = panel
	if hs, err = ResolveHubServer(r); err != nil || hs.CertFile != "/env/c.pem" || hs.KeyFile != "/panel/k.pem" {
		t.Fatalf("env over panel: %+v, %v", hs, err)
	}

	// One side only, from the panel, is still an error.
	r = hubEnv(map[string]string{"MINION_DATA_DIR": "/d"})
	r.Panel = MapSource{KeyTLSKeyFile: "/panel/k.pem"}
	if _, err := ResolveHubServer(r); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("panel with only a key: %v", err)
	}
}
