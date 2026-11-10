package config

import (
	"strings"
	"testing"
)

func TestCoreSettingsDeclared(t *testing.T) {
	want := map[string]string{
		KeyDataDir:   "MINION_DATA_DIR",
		KeyStateDir:  "MINION_STATE_DIR",
		KeyLogLevel:  "MINION_LOG_LEVEL",
		KeyLogFormat: "MINION_LOG_FORMAT",
	}
	for key, env := range want {
		s, ok := Default.Lookup(key)
		if !ok {
			t.Fatalf("%s not declared", key)
		}
		if s.EnvName() != env {
			t.Errorf("%s env = %s, want %s", key, s.EnvName(), env)
		}
		if s.Help == "" {
			t.Errorf("%s has no help text", key)
		}
	}
}

func TestCoreDefaults(t *testing.T) {
	r := &Resolver{Role: ScopeWorker, LookupEnv: envMap(nil)}
	for key, want := range map[string]string{KeyLogLevel: "info", KeyLogFormat: "auto"} {
		got, err := r.Resolve(key)
		if err != nil || got.Value != want {
			t.Errorf("%s = %q (err %v), want %q", key, got.Value, err, want)
		}
	}
	for _, key := range []string{KeyDataDir, KeyStateDir} {
		got, err := r.Resolve(key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if !strings.Contains(got.Value, "minion") {
			t.Errorf("%s default = %q", key, got.Value)
		}
	}
}

func TestCoreOverridesAndValidation(t *testing.T) {
	r := &Resolver{Role: ScopeHub, LookupEnv: envMap(map[string]string{
		"MINION_DATA_DIR":   "/srv/data",
		"MINION_LOG_FORMAT": "text",
		"MINION_LOG_LEVEL":  "verbose",
	})}
	if got, _ := r.Resolve(KeyDataDir); got.Value != "/srv/data" || got.Source != SourceEnv {
		t.Errorf("data_dir = %+v", got)
	}
	if got, _ := r.Resolve(KeyLogFormat); got.Value != "text" {
		t.Errorf("log_format = %+v", got)
	}
	if _, err := r.Resolve(KeyLogLevel); err == nil || !strings.Contains(err.Error(), "debug, info, warn, error") {
		t.Errorf("log_level invalid value: %v", err)
	}
	// log_format accepts exactly auto|json|text.
	for _, v := range []string{"auto", "json", "text"} {
		r.LookupEnv = envMap(map[string]string{"MINION_LOG_FORMAT": v})
		if _, err := r.Resolve(KeyLogFormat); err != nil {
			t.Errorf("log_format %q: %v", v, err)
		}
	}
	r.LookupEnv = envMap(map[string]string{"MINION_LOG_FORMAT": "xml"})
	if _, err := r.Resolve(KeyLogFormat); err == nil {
		t.Error("log_format xml accepted")
	}
}

func TestCoreFlagNames(t *testing.T) {
	for key, flag := range map[string]string{KeyDataDir: "data-dir", KeyStateDir: "state-dir", KeyLogLevel: "log-level", KeyLogFormat: "log-format"} {
		s, _ := Default.Lookup(key)
		if s.FlagName() != flag || !s.HasFlag() {
			t.Errorf("%s: flag %q hasFlag %v", key, s.FlagName(), s.HasFlag())
		}
	}
}

func TestRedactedValueNotLeakedByDisplay(t *testing.T) {
	r := Resolved{Setting: Setting{Key: "k", Secret: true}, Value: "v", Source: SourceFile}
	if r.Display() != Redacted {
		t.Fatal("secret displayed")
	}
	if (Resolved{Setting: Setting{Key: "k", Secret: true}}).Display() != "" {
		t.Fatal("unset secret should display empty")
	}
}
