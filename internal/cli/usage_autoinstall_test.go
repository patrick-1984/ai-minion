package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func autoInstallFixture(t *testing.T, accept bool, env map[string]string) (home, stateDir string, res *config.Resolver) {
	t.Helper()
	st, _ := claudeFixtureEnrollment(t, accept)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	old := agent.ClaudeManagedSettingsFile
	agent.ClaudeManagedSettingsFile = filepath.Join(t.TempDir(), "managed-settings.json")
	t.Cleanup(func() { agent.ClaudeManagedSettingsFile = old; agent.SetClaudeExporterIssue("") })
	res = &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	return home, st.Dir(), res
}

func TestAutoInstallClaudeExporterKeepsStatusLineAndIsIdempotent(t *testing.T) {
	home, state, res := autoInstallFixture(t, true, nil)
	profile := filepath.Join(home, ".claude")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(profile, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"my-line"}}`))
	for i := 0; i < 2; i++ {
		autoInstallClaudeExporter(context.Background(), res, state, func(string) (string, bool) { return "", false })
	}
	raw := readClaudeFixture(t, profile, "settings.json")
	m, err := readClaudeUsageManifest(filepath.Join(profile, claudeUsageManifest))
	if err != nil || m == nil || m.PreviousLine != "my-line" || !bytes.Contains(raw, []byte(claudeUsageMarker)) {
		t.Fatalf("not installed with the previous line kept: %+v %v %s", m, err, raw)
	}
	var s struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(raw, &s) != nil || len(s.Hooks) == 0 || agent.ClaudeExporterIssue() != "" {
		t.Fatalf("hooks %v issue %q", s.Hooks, agent.ClaudeExporterIssue())
	}
	autoInstallClaudeExporter(context.Background(), res, state, func(string) (string, bool) { return "", false })
	if !bytes.Equal(raw, readClaudeFixture(t, profile, "settings.json")) {
		t.Fatal("a repeated run changed the settings")
	}
}

func TestAutoInstallClaudeExporterRespectsOptOutManagedSettingsConsentAndNoProfile(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	settings := []byte(`{"permissions":{}}`)
	prep := func(t *testing.T, accept bool, env map[string]string) (string, string, *config.Resolver) {
		home, state, res := autoInstallFixture(t, accept, env)
		profile := filepath.Join(home, ".claude")
		if err := os.MkdirAll(profile, 0o700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(profile, "settings.json"), settings)
		return profile, state, res
	}
	unchanged := func(t *testing.T, profile string) {
		t.Helper()
		if !bytes.Equal(settings, readClaudeFixture(t, profile, "settings.json")) {
			t.Fatal("settings were modified")
		}
		if _, err := os.Stat(filepath.Join(profile, claudeUsageManifest)); err == nil {
			t.Fatal("manifest written")
		}
	}

	profile, state, res := prep(t, true, map[string]string{"MINION_USAGE_CLAUDE_EXPORTER": "off"})
	autoInstallClaudeExporter(context.Background(), res, state, lookup)
	unchanged(t, profile)
	if agent.ClaudeExporterIssue() != ops.UsageExporterOff {
		t.Fatalf("opt-out issue %q", agent.ClaudeExporterIssue())
	}

	profile, state, res = prep(t, true, nil)
	mustWrite(t, agent.ClaudeManagedSettingsFile, []byte(`{"statusLine":{"type":"command","command":"corp"}}`))
	autoInstallClaudeExporter(context.Background(), res, state, lookup)
	unchanged(t, profile)
	if agent.ClaudeExporterIssue() != ops.UsageExporterOverridden {
		t.Fatalf("managed issue %q", agent.ClaudeExporterIssue())
	}

	profile, state, res = prep(t, false, nil) // monitoring not accepted
	if !autoInstallClaudeExporter(context.Background(), res, state, lookup) {
		t.Fatal("a profile waiting for the usage consent is not reported as pending, so the next try waits the long interval")
	}
	unchanged(t, profile)

	_, state, res = autoInstallFixture(t, true, nil) // no Claude profile at all
	if autoInstallClaudeExporter(context.Background(), res, state, lookup) {
		t.Fatal("pending without a profile")
	}
	if agent.ClaudeExporterIssue() != "" {
		t.Fatalf("issue without a profile: %q", agent.ClaudeExporterIssue())
	}

	profile, state, res = prep(t, true, nil)
	mustWrite(t, filepath.Join(profile, "settings.json"), []byte(`{"statusLine":"not-an-object"}`))
	autoInstallClaudeExporter(context.Background(), res, state, lookup)
	if agent.ClaudeExporterIssue() != ops.UsageExporterConflict || !strings.Contains(string(readClaudeFixture(t, profile, "settings.json")), "not-an-object") {
		t.Fatalf("conflict issue %q", agent.ClaudeExporterIssue())
	}
}
