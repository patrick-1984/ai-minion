package config

import (
	"errors"
	"strings"
	"testing"
)

func clientResolver(env map[string]string, flags, file MapSource) *Resolver {
	return &Resolver{
		Registry:  Client,
		Role:      ScopeWorker,
		Flags:     flags,
		File:      file,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}
}

func TestClientSettingsAreDeclaredForTheClientOnly(t *testing.T) {
	for _, key := range []string{KeyHubURL, KeyAPIKey, KeyHubWorker} {
		if _, ok := Default.Lookup(key); ok {
			t.Errorf("%s is declared for the Worker and the Hub: the client settings must not reach `worker run` or `worker config`", key)
		}
		if _, ok := Client.Lookup(key); !ok {
			t.Errorf("%s is not declared for the client", key)
		}
	}
	want := map[string]string{KeyHubURL: "MINION_HUB_URL", KeyAPIKey: "MINION_API_KEY", KeyHubCAFile: "MINION_HUB_CA", KeyHubInsecure: "MINION_HUB_INSECURE", KeyHubWorker: "MINION_HUB_WORKER"}
	for key, env := range want {
		s, ok := Client.Lookup(key)
		if !ok || s.EnvName() != env {
			t.Errorf("%s: %+v", key, s)
		}
	}
	key, _ := Client.Lookup(KeyAPIKey)
	if !key.Secret || key.HasFlag() || key.PlainEnvAllowed || key.ArgvAllowedWithWarning || key.FileEnvName() != APIKeyFileEnv {
		t.Errorf("the API key is not file-only: %+v", key)
	}
}

func TestClientSettingsHelpHasNoSecretsOrExamplesOfKeys(t *testing.T) {
	for _, s := range Client.All() {
		if strings.Contains(s.Help, "mn_") && !strings.Contains(s.Help, "(mn_...)") {
			t.Errorf("%s help: %q", s.Key, s.Help)
		}
		if s.Default != "" && s.Secret {
			t.Errorf("%s: a secret with a default", s.Key)
		}
	}
}

func TestResolveClientHubPrecedenceFlagEnvFileDefault(t *testing.T) {
	file := MapSource{KeyHubURL: "https://file.example", KeyHubCAFile: "/file/ca.pem", KeyHubWorker: "01930000-0000-7000-8000-0000000000a1"}
	env := map[string]string{"MINION_HUB_URL": "https://env.example", "MINION_HUB_CA": "/env/ca.pem"}
	flags := MapSource{KeyHubURL: "https://flag.example"}

	got, err := ResolveClientHub(clientResolver(env, flags, file))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://flag.example" || got.CAFile != "/env/ca.pem" || got.Worker != "01930000-0000-7000-8000-0000000000a1" || got.Insecure {
		t.Errorf("flag > env > file > default: %+v", got)
	}
	got, err = ResolveClientHub(clientResolver(env, nil, file))
	if err != nil || got.URL != "https://env.example" {
		t.Errorf("env over file: %+v, %v", got, err)
	}
	got, err = ResolveClientHub(clientResolver(nil, nil, file))
	if err != nil || got.URL != "https://file.example" || got.CAFile != "/file/ca.pem" {
		t.Errorf("file: %+v, %v", got, err)
	}
	got, err = ResolveClientHub(clientResolver(nil, nil, nil))
	if err != nil || got.Configured() || got.Insecure {
		t.Errorf("defaults: %+v, %v", got, err)
	}
	got, err = ResolveClientHub(clientResolver(map[string]string{"MINION_HUB_INSECURE": "true"}, nil, nil))
	if err != nil || !got.Insecure {
		t.Errorf("insecure from the environment: %+v, %v", got, err)
	}
	if _, err := ResolveClientHub(clientResolver(map[string]string{"MINION_HUB_INSECURE": "maybe"}, nil, nil)); err == nil {
		t.Error("an invalid insecure value was accepted")
	}
}

func TestResolveClientHubTakesTheKeyOnlyFromTheFileVariable(t *testing.T) {
	const secret = "mn_CANARYSECRETVALUE0123456789abcdefg"
	got, err := ResolveClientHub(clientResolver(map[string]string{"MINION_API_KEY_FILE": " /run/secrets/minion_key "}, nil, nil))
	if err != nil || got.APIKeyFile != "/run/secrets/minion_key" || !got.Configured() {
		t.Errorf("file variable: %+v, %v", got, err)
	}
	// The plain variable is refused, without repeating the value.
	_, err = ResolveClientHub(clientResolver(map[string]string{"MINION_API_KEY": secret}, nil, nil))
	if !errors.Is(err, ErrAPIKeyNotAccepted) || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "MINION_API_KEY_FILE") {
		t.Errorf("plain env: %v", err)
	}
	// So is a key in the config file.
	_, err = ResolveClientHub(clientResolver(nil, nil, MapSource{KeyAPIKey: secret}))
	if !errors.Is(err, ErrAPIKeyNotAccepted) || strings.Contains(err.Error(), secret) {
		t.Errorf("config file: %v", err)
	}
	// A flag value for the key cannot be offered at all.
	got, err = ResolveClientHub(clientResolver(nil, MapSource{KeyAPIKey: secret}, nil))
	if err != nil || got.APIKeyFile != "" {
		t.Errorf("a flag source for the key was used: %+v, %v", got, err)
	}
}

func TestValidateHubURL(t *testing.T) {
	for _, tc := range []struct {
		in, out string
		ok      bool
	}{
		{"", "", true},
		{"  https://hub.example  ", "https://hub.example", true},
		{"https://hub.example:8443/prefix", "https://hub.example:8443/prefix", true},
		{"http://localhost:8080", "http://localhost:8080", true},
		{"wss://hub.example", "", false},
		{"hub.example", "", false},
		{"https://", "", false},
		{"https://u:p@hub.example", "", false},
		{"https://hub.example/?x=1", "", false},
		{"https://hub.example/#f", "", false},
	} {
		got, err := ValidateHubURL(tc.in)
		if (err == nil) != tc.ok || got != tc.out {
			t.Errorf("%q: %q, %v", tc.in, got, err)
		}
		if err != nil && (!errors.Is(err, ErrInvalidHubURL) || strings.Contains(err.Error(), "p@")) {
			t.Errorf("%q: %v", tc.in, err)
		}
	}
}

// A credential typed into the Hub URL is refused without being repeated, in
// whatever form: host, path, other letter case, percent-encoded.
func TestHubURLThatLooksLikeAKeyIsRefusedAndNeverRepeated(t *testing.T) {
	const key = "mn_abcdefghijklmnopqrstuvwxyzabcdef"
	for _, raw := range []string{
		"https://" + key + ".invalid",
		"https://" + key + ".invalid:8443/x",
		"https://hub.example/" + key,
		"https://" + "MN_ABCDEFGHIJKLMNOPQRSTUVWXYZABCDEF" + ".invalid",
		"https://%6dn_abcdefghijklmnopqrstuvwxyzabcdef.invalid",
		"https://hub.example/%6dn_abcdefghijklmnopqrstuvwxyzabcdef",
	} {
		_, err := ValidateHubURL(raw)
		if !errors.Is(err, ErrInvalidHubURL) || strings.Contains(strings.ToLower(err.Error()), "abcdefghijklmnop") {
			t.Errorf("%s: %v", raw, err)
		}
		_, err = ResolveClientHub(clientResolver(map[string]string{"MINION_HUB_URL": raw}, nil, nil))
		if !errors.Is(err, ErrInvalidHubURL) || strings.Contains(strings.ToLower(err.Error()), "abcdefghijklmnop") {
			t.Errorf("resolve %s: %v", raw, err)
		}
	}
	for _, ok := range []string{"https://hub.example:8443", "https://hub.example/mn", "https://mn-hub.example/mn_short", "http://127.0.0.1:8080/prefix"} {
		if _, err := ValidateHubURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}
