package config

import (
	"strings"
	"testing"
)

func TestWorkerDebugSettings(t *testing.T) {
	for key, env := range map[string]string{KeyDebug: "MINION_DEBUG", KeyAllowDebugRemote: "MINION_ALLOW_DEBUG_REMOTE"} {
		s, ok := Default.Lookup(key)
		if !ok || s.EnvName() != env || s.Scope != ScopeWorker || s.Default != "false" || !s.HasFlag() {
			t.Errorf("%s = %+v (ok %v)", key, s, ok)
		}
	}
	if s, _ := Default.Lookup(KeyAllowDebugRemote); s.FlagName() != "allow-debug-remote" {
		t.Errorf("flag = %s", s.FlagName())
	}
	// Default off.
	r := &Resolver{Role: ScopeWorker, LookupEnv: envMap(nil)}
	if on, err := ResolveBool(r, KeyAllowDebugRemote); err != nil || on {
		t.Fatalf("default = %v, %v", on, err)
	}
	// The environment, with 1 as true.
	r = &Resolver{Role: ScopeWorker, LookupEnv: envMap(map[string]string{"MINION_ALLOW_DEBUG_REMOTE": "1"})}
	if on, err := ResolveBool(r, KeyAllowDebugRemote); err != nil || !on {
		t.Fatalf("env 1 = %v, %v", on, err)
	}
	// A bad value is an error, never a silent default.
	r = &Resolver{Role: ScopeWorker, LookupEnv: envMap(map[string]string{"MINION_ALLOW_DEBUG_REMOTE": "yes"})}
	if _, err := ResolveBool(r, KeyAllowDebugRemote); err == nil {
		t.Fatal("yes accepted")
	}
	// The Hub has neither setting.
	hub := &Resolver{Role: ScopeHub, LookupEnv: envMap(nil)}
	if _, err := hub.Resolve(KeyAllowDebugRemote); err == nil {
		t.Fatal("the Hub resolved allow_debug_remote")
	}
}

func TestResolveLogLevelDebugSwitch(t *testing.T) {
	cases := []struct {
		env  map[string]string
		role Scope
		want string
	}{
		{map[string]string{"MINION_DEBUG": "1"}, ScopeWorker, "debug"},
		{map[string]string{"MINION_DEBUG": "true", "MINION_LOG_LEVEL": "warn"}, ScopeWorker, "debug"},
		{map[string]string{"MINION_DEBUG": "0", "MINION_LOG_LEVEL": "warn"}, ScopeWorker, "warn"},
		{nil, ScopeWorker, "info"},
		// The Hub ignores MINION_DEBUG: the switch is a Worker setting.
		{map[string]string{"MINION_DEBUG": "1"}, ScopeHub, "info"},
	}
	for _, c := range cases {
		r := &Resolver{Role: c.role, LookupEnv: envMap(c.env)}
		got, err := ResolveLogLevel(r, nil)
		if err != nil || got != c.want {
			t.Errorf("%v (%s) = %q, %v; want %q", c.env, c.role, got, err, c.want)
		}
	}
	r := &Resolver{Role: ScopeWorker, LookupEnv: envMap(map[string]string{"MINION_DEBUG": "loud"})}
	if _, err := ResolveLogLevel(r, nil); err == nil || !strings.Contains(err.Error(), "debug") {
		t.Fatalf("bad debug value: %v", err)
	}
}
