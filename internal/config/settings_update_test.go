package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateSettingsDeclared(t *testing.T) {
	cases := []struct{ key, env, flag, def string }{
		{KeyChannel, "MINION_CHANNEL", "channel", "latest"},
		{KeyUpdateAPIURL, "MINION_UPDATE_API_URL", "update-api-url", "https://api.github.com"},
	}
	for _, tc := range cases {
		s, ok := Default.Lookup(tc.key)
		if !ok {
			t.Fatalf("setting %q is not declared", tc.key)
		}
		if s.EnvName() != tc.env || s.FlagName() != tc.flag || s.Default != tc.def {
			t.Errorf("%s: env %s flag %s default %q", tc.key, s.EnvName(), s.FlagName(), s.Default)
		}
		if s.Scope != ScopeWorker || s.Secret || !s.HasFlag() || s.Help == "" {
			t.Errorf("%s: scope %s secret %v flag %v help %q", tc.key, s.Scope, s.Secret, s.HasFlag(), s.Help)
		}
	}
	s, ok := Default.Lookup(KeyUpdateCacheDir)
	if !ok || s.EnvName() != "MINION_UPDATE_CACHE_DIR" || s.FlagName() != "update-cache-dir" || s.DefaultFunc == nil {
		t.Fatalf("update_cache_dir: %+v", s)
	}
}

func TestUpdateSettingsAreWorkerScope(t *testing.T) {
	for _, s := range Default.ForScope(ScopeHub) {
		switch s.Key {
		case KeyChannel, KeyUpdateAPIURL, KeyUpdateCacheDir:
			t.Errorf("the Hub can resolve %s", s.Key)
		}
	}
	found := 0
	for _, s := range Default.ForScope(ScopeWorker) {
		switch s.Key {
		case KeyChannel, KeyUpdateAPIURL, KeyUpdateCacheDir:
			found++
		}
	}
	if found != 3 {
		t.Errorf("the Worker sees %d of 3 update settings", found)
	}
}

func TestUpdateSettingsResolve(t *testing.T) {
	cache := t.TempDir()
	env := map[string]string{
		"MINION_CHANNEL":          "1.4",
		"MINION_UPDATE_CACHE_DIR": cache,
		"XDG_CACHE_HOME":          cache,
	}
	r := &Resolver{Role: ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	got, err := r.Resolve(KeyChannel)
	if err != nil || got.Value != "1.4" || got.Source != SourceEnv {
		t.Errorf("channel = %+v, %v", got, err)
	}
	got, err = r.Resolve(KeyUpdateAPIURL)
	if err != nil || got.Value != "https://api.github.com" || got.Source != SourceDefault {
		t.Errorf("api url = %+v, %v", got, err)
	}
	got, err = r.Resolve(KeyUpdateCacheDir)
	if err != nil || got.Value != cache {
		t.Errorf("cache dir = %+v, %v", got, err)
	}
	delete(env, "MINION_UPDATE_CACHE_DIR")
	t.Setenv("XDG_CACHE_HOME", cache)
	got, err = r.Resolve(KeyUpdateCacheDir)
	if err != nil || !filepath.IsAbs(got.Value) || !strings.HasSuffix(got.Value, filepath.Join("minion", "update")) {
		t.Errorf("default cache dir = %+v, %v", got, err)
	}
}
