package config

import (
	"strings"
	"testing"
)

func mustPanic(t *testing.T, wantSubstr string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q, got none", wantSubstr)
		}
		if msg, _ := r.(string); !strings.Contains(msg, wantSubstr) {
			t.Fatalf("panic = %v, want substring %q", r, wantSubstr)
		}
	}()
	fn()
}

func TestNamesDeriveFromKey(t *testing.T) {
	s := Setting{Key: "server", Scope: ScopeWorker}
	if got := s.EnvName(); got != "MINION_SERVER" {
		t.Errorf("EnvName = %q, want MINION_SERVER", got)
	}
	if got := s.FlagName(); got != "server" {
		t.Errorf("FlagName = %q, want server", got)
	}
	multi := Setting{Key: "admin_group_claim", Scope: ScopeHub}
	if got := multi.EnvName(); got != "MINION_ADMIN_GROUP_CLAIM" {
		t.Errorf("EnvName = %q", got)
	}
	if got := multi.FlagName(); got != "admin-group-claim" {
		t.Errorf("FlagName = %q, want kebab-case", got)
	}
	if got := (Setting{Key: "admin_password"}).FileEnvName(); got != "MINION_ADMIN_PASSWORD_FILE" {
		t.Errorf("FileEnvName = %q", got)
	}
}

func TestDeclareAndLookup(t *testing.T) {
	r := NewRegistry()
	r.Declare(Setting{Key: "server", Scope: ScopeWorker})
	r.Declare(Setting{Key: "base_url", Scope: ScopeHub})
	r.Declare(Setting{Key: "log_x", Scope: ScopeAny})

	if _, ok := r.Lookup("server"); !ok {
		t.Fatal("server not found")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("unexpected setting")
	}
	var keys []string
	for _, s := range r.All() {
		keys = append(keys, s.Key)
	}
	if got := strings.Join(keys, ","); got != "base_url,log_x,server" {
		t.Errorf("All = %s, want sorted keys", got)
	}
	keys = nil
	for _, s := range r.ForScope(ScopeHub) {
		keys = append(keys, s.Key)
	}
	if got := strings.Join(keys, ","); got != "base_url,log_x" {
		t.Errorf("ForScope(hub) = %s", got)
	}
}

func TestDeclarePanics(t *testing.T) {
	cases := []struct {
		name string
		want string
		s    Setting
	}{
		{"env without prefix", "does not start with MINION_", Setting{Key: "a", Scope: ScopeHub, Env: "SERVER"}},
		{"env wrong prefix", "does not start with MINION_", Setting{Key: "a", Scope: ScopeHub, Env: "MINIONSERVER"}},
		{"env lower case", "not UPPER_SNAKE", Setting{Key: "a", Scope: ScopeHub, Env: "MINION_server"}},
		{"env dash", "not UPPER_SNAKE", Setting{Key: "a", Scope: ScopeHub, Env: "MINION_A-B"}},
		{"env double underscore", "not UPPER_SNAKE", Setting{Key: "a", Scope: ScopeHub, Env: "MINION_A__B"}},
		{"env trailing underscore", "not UPPER_SNAKE", Setting{Key: "a", Scope: ScopeHub, Env: "MINION_A_"}},
		{"env bare prefix", "not UPPER_SNAKE", Setting{Key: "a", Scope: ScopeHub, Env: "MINION_"}},
		{"env ends in _FILE", "must not end in _FILE", Setting{Key: "a", Scope: ScopeHub, Env: "MINION_A_FILE"}},
		{"key upper case", "not lower_snake_case", Setting{Key: "Server", Scope: ScopeHub}},
		{"key dash", "not lower_snake_case", Setting{Key: "a-b", Scope: ScopeHub}},
		{"key empty", "not lower_snake_case", Setting{Scope: ScopeHub}},
		{"bad scope", "invalid scope", Setting{Key: "a", Scope: "mars"}},
		{"secret default", "secret must not have a default", Setting{Key: "a", Scope: ScopeHub, Secret: true, Default: "x"}},
		{"exception on non-secret", "secret exceptions", Setting{Key: "a", Scope: ScopeHub, PlainEnvAllowed: true}},
		{"default and func", "mutually exclusive", Setting{Key: "a", Scope: ScopeHub, Default: "x", DefaultFunc: func() (string, error) { return "", nil }}},
		{"default not a choice", "is not one of", Setting{Key: "a", Scope: ScopeHub, Default: "z", Choices: []string{"x", "y"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustPanic(t, tc.want, func() { NewRegistry().Declare(tc.s) })
		})
	}
}

func TestDuplicatesPanic(t *testing.T) {
	r := NewRegistry()
	r.Declare(Setting{Key: "server", Scope: ScopeWorker})
	mustPanic(t, "duplicate setting key", func() {
		r.Declare(Setting{Key: "server", Scope: ScopeHub})
	})
	mustPanic(t, "env name MINION_SERVER is already used", func() {
		r.Declare(Setting{Key: "other", Scope: ScopeHub, Env: "MINION_SERVER"})
	})
}

func TestExplicitEnvOverride(t *testing.T) {
	r := NewRegistry()
	r.Declare(Setting{Key: "x", Scope: ScopeHub, Env: "MINION_CUSTOM_NAME"})
	s, _ := r.Lookup("x")
	if s.EnvName() != "MINION_CUSTOM_NAME" || s.FlagName() != "custom-name" {
		t.Fatalf("got env %q flag %q", s.EnvName(), s.FlagName())
	}
}

func TestHasFlag(t *testing.T) {
	cases := []struct {
		name string
		s    Setting
		want bool
	}{
		{"worker plain", Setting{Key: "a", Scope: ScopeWorker}, true},
		{"any plain", Setting{Key: "a", Scope: ScopeAny}, true},
		{"hub plain", Setting{Key: "a", Scope: ScopeHub}, false},
		{"worker secret", Setting{Key: "a", Scope: ScopeWorker, Secret: true}, false},
		{"worker secret argv exception", Setting{Key: "a", Scope: ScopeWorker, Secret: true, ArgvAllowedWithWarning: true}, true},
	}
	for _, tc := range cases {
		if got := tc.s.HasFlag(); got != tc.want {
			t.Errorf("%s: HasFlag = %v, want %v", tc.name, got, tc.want)
		}
	}
}
