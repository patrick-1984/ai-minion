package config

import (
	"errors"
	"strings"
	"testing"
)

func TestWorkerHubSettingsDeclared(t *testing.T) {
	want := map[string]struct{ flag, env string }{
		KeyServer:      {"server", "MINION_SERVER"},
		KeyHubCAFile:   {"hub-ca", "MINION_HUB_CA"},
		KeyHubInsecure: {"hub-insecure", "MINION_HUB_INSECURE"},
	}
	for key, w := range want {
		s, ok := Default.Lookup(key)
		if !ok {
			t.Fatalf("%s not declared", key)
		}
		if s.Scope != ScopeWorker || !s.HasFlag() || s.FlagName() != w.flag || s.EnvName() != w.env || s.Help == "" || s.Secret {
			t.Errorf("%s: scope %s flag %q env %q secret %v help %q", key, s.Scope, s.FlagName(), s.EnvName(), s.Secret, s.Help)
		}
	}
}

func TestResolveWorkerHubDefaultsToSolo(t *testing.T) {
	wh, err := ResolveWorkerHub(workerResolver(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if wh != (WorkerHub{}) {
		t.Fatalf("defaults = %+v, want no server, no CA file, verification on", wh)
	}
}

func TestResolveWorkerHubPrecedence(t *testing.T) {
	wh, err := ResolveWorkerHub(workerResolver(
		map[string]string{KeyServer: " https://flag.example:8443 "},
		map[string]string{"MINION_SERVER": "https://env.example", "MINION_HUB_CA": "/etc/ca.pem"},
		map[string]string{KeyServer: "https://file.example", KeyHubCAFile: "/file/ca.pem", KeyHubInsecure: "true"},
	))
	if err != nil {
		t.Fatal(err)
	}
	want := WorkerHub{Server: "https://flag.example:8443", CAFile: "/etc/ca.pem", Insecure: true}
	if wh != want {
		t.Fatalf("hub = %+v, want %+v", wh, want)
	}
}

func TestResolveWorkerHubFromTheFileAlone(t *testing.T) {
	wh, err := ResolveWorkerHub(workerResolver(nil, nil, map[string]string{KeyServer: "wss://hub.example/minion"}))
	if err != nil || wh.Server != "wss://hub.example/minion" {
		t.Fatalf("hub = %+v, %v", wh, err)
	}
}

func TestValidateServerURL(t *testing.T) {
	for _, ok := range []string{"", "  ", "https://hub.example", "http://127.0.0.1:8080", "wss://hub/x", "ws://h:1", "HTTPS://Hub.example/prefix"} {
		if _, err := ValidateServerURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"hub.example", "ftp://hub", "https://", "https://user:s3cret@hub.example", "https://user@hub.example", "://x", "https://[::1"} {
		_, err := ValidateServerURL(bad)
		if !errors.Is(err, ErrInvalidServer) {
			t.Errorf("%q: err = %v, want ErrInvalidServer", bad, err)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%q: the error repeats a credential: %v", bad, err)
		}
	}
}

func TestResolveWorkerHubRejectsABadServerAndNamesTheSource(t *testing.T) {
	_, err := ResolveWorkerHub(workerResolver(nil, map[string]string{"MINION_SERVER": "hub.example"}, nil))
	if !errors.Is(err, ErrInvalidServer) || !strings.Contains(err.Error(), "env") {
		t.Fatalf("err = %v", err)
	}
	if _, err := ResolveWorkerHub(workerResolver(nil, map[string]string{"MINION_HUB_INSECURE": "maybe"}, nil)); err == nil {
		t.Fatal("an invalid hub_insecure must be refused")
	}
}
