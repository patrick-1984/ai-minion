package config

import (
	"strings"
	"testing"
)

func TestJoinIsDeclaredAsASecretWithFlagAndPlainEnvironment(t *testing.T) {
	s, ok := Default.Lookup(KeyJoin)
	if !ok {
		t.Fatal("join is not declared")
	}
	if s.Scope != ScopeWorker || !s.Secret || !s.HasFlag() || !s.ArgvAllowedWithWarning || !s.PlainEnvAllowed || s.Default != "" {
		t.Fatalf("setting = %+v", s)
	}
	if s.FlagName() != "join" || s.EnvName() != "MINION_JOIN" || s.FileEnvName() != "MINION_JOIN_FILE" {
		t.Fatalf("flag --%s env %s file env %s", s.FlagName(), s.EnvName(), s.FileEnvName())
	}
}

func TestJoinResolvesFromFlagEnvAndFileEnvAndNeverDisplays(t *testing.T) {
	const tok = "mnj1-c2VjcmV0"
	for name, res := range map[string]*Resolver{
		"flag":     {Role: ScopeWorker, Flags: MapSource{KeyJoin: tok}, LookupEnv: envMap(nil)},
		"env":      {Role: ScopeWorker, LookupEnv: envMap(map[string]string{"MINION_JOIN": tok})},
		"env file": {Role: ScopeWorker, LookupEnv: envMap(map[string]string{"MINION_JOIN_FILE": "/run/secrets/join"}), ReadFile: fileMap(map[string]string{"/run/secrets/join": tok + "\n"})},
	} {
		r, err := res.Resolve(KeyJoin)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Value != tok {
			t.Errorf("%s: value %q", name, r.Value)
		}
		if strings.Contains(r.String(), tok) || strings.Contains(r.GoString(), tok) || r.Display() != Redacted {
			t.Errorf("%s: printed the token: %s", name, r)
		}
		if (name == "flag") != (r.Warning != "") {
			t.Errorf("%s: warning %q", name, r.Warning)
		}
		if r.Warning != "" && (!strings.Contains(r.Warning, "--join") || strings.Contains(r.Warning, tok)) {
			t.Errorf("warning %q", r.Warning)
		}
	}
}
