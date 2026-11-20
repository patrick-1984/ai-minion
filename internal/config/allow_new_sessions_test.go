package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAllowNewSessionsFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), WorkerConfigFile)
	env := map[string]string{}
	res := &Resolver{Role: ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, file, env string
		want            bool
	}{
		{"default", "", "", true},
		{"file off", `{"allow_new_sessions":"off"}`, "", false},
		{"file on", `{"allow_new_sessions":"on"}`, "", true},
		{"env off", "", "off", false},
		{"env on does not lift a file block", `{"allow_new_sessions":"off"}`, "on", false},
		{"file on does not lift an env block", `{"allow_new_sessions":"on"}`, "off", false},
		{"garbage in the file blocks", `{"allow_new_sessions":"maybe"}`, "", false},
		{"garbage in the environment blocks", "", "yes", false},
	} {
		env = map[string]string{}
		if tc.env != "" {
			env["MINION_ALLOW_NEW_SESSIONS"] = tc.env
		}
		_ = os.Remove(path)
		if tc.file != "" {
			write(tc.file)
		}
		if got := ResolveAllowNewSessions(res, path); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The file is read again at every call: a saved change applies at once.
	env = map[string]string{}
	write(`{"allow_new_sessions":"off"}`)
	if ResolveAllowNewSessions(res, path) {
		t.Fatal("blocked file allowed")
	}
	write(`{"allow_new_sessions":"on"}`)
	if !ResolveAllowNewSessions(res, path) {
		t.Fatal("the saved on was not read again")
	}
	// A config file that cannot be read blocks.
	write(`{`)
	if ResolveAllowNewSessions(res, path) {
		t.Fatal("an unreadable config file allowed new sessions")
	}
	if !ResolveAllowNewSessions(nil, path) {
		t.Fatal("no resolver (a unit test) must allow")
	}
}
