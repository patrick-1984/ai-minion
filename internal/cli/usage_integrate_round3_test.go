package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func round3Fixture(t *testing.T) (claudeUsageIntegration, string) {
	t.Helper()
	st, _ := claudeFixtureEnrollment(t, true)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell, powerShell, err := claudeUsageShell(os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	return claudeUsageIntegration{Version: 1, Binary: binary, StateDir: st.Dir(), Shell: shell, PowerShell: powerShell}, st.Dir()
}

func TestInstallRefreshesADiscoveredAccountOnFirstLoginAndSwitch(t *testing.T) {
	m, _ := round3Fixture(t)
	profile := t.TempDir()
	manifest := filepath.Join(profile, claudeUsageManifest)
	var found *ops.UsageAccount
	discover := func(context.Context, string) (ops.UsageAccount, bool) {
		if found == nil {
			return ops.UsageAccount{}, false
		}
		return *found, true
	}
	install := func() {
		t.Helper()
		if _, err := installClaudeUsage(context.Background(), profile, m, discover); err != nil {
			t.Fatalf("install: %v", err)
		}
	}
	install() // signed out: no account yet
	if got, _ := readClaudeUsageManifest(manifest); got == nil || got.Account != "" {
		t.Fatalf("manifest %+v", got)
	}
	first := ops.UsageAccount{ID: ops.UsageID("acct", "first"), Provider: "Anthropic", AuthMode: "subscription", Label: "a@example.com", Plan: "max"}
	found = &first
	install() // first login
	if got, _ := readClaudeUsageManifest(manifest); got == nil || got.Account != first.ID || got.Label != "a@example.com" {
		t.Fatalf("first login not picked up: %+v", got)
	}
	second := ops.UsageAccount{ID: ops.UsageID("acct", "second"), Provider: "Anthropic", AuthMode: "subscription", Label: "b@example.com"}
	found = &second
	install() // account switch
	if got, _ := readClaudeUsageManifest(manifest); got == nil || got.Account != second.ID || got.Label != "b@example.com" {
		t.Fatalf("account switch not picked up: %+v", got)
	}
	found = nil
	install() // discovery fails: the known account stays
	if got, _ := readClaudeUsageManifest(manifest); got == nil || got.Account != second.ID {
		t.Fatalf("a failed discovery dropped the account: %+v", got)
	}
}

func TestInstallKeepsAnExplicitBindingAgainstDiscovery(t *testing.T) {
	m, _ := round3Fixture(t)
	profile := t.TempDir()
	m.Account = ops.UsageID("acct", "explicit")
	if _, err := installClaudeUsage(context.Background(), profile, m, nil); err != nil {
		t.Fatal(err)
	}
	m.Account = ""
	other := ops.UsageAccount{ID: ops.UsageID("acct", "other"), Provider: "Anthropic", AuthMode: "subscription"}
	_, err := installClaudeUsage(context.Background(), profile, m, func(context.Context, string) (ops.UsageAccount, bool) { return other, true })
	if err != nil {
		t.Fatalf("an automatic pass must not fail against an explicit binding: %v", err)
	}
	if got, _ := readClaudeUsageManifest(filepath.Join(profile, claudeUsageManifest)); got.Account != ops.UsageID("acct", "explicit") {
		t.Fatalf("explicit binding replaced: %+v", got)
	}
}

func TestRemoveThroughADirectoryAliasFindsTheInstalledCommands(t *testing.T) {
	m, _ := round3Fixture(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	settings := `{"statusLine":{"type":"command","command":"my-line"}}`
	mustWrite(t, filepath.Join(real, "settings.json"), []byte(settings))
	if _, err := installClaudeUsage(context.Background(), link, m, nil); err != nil {
		t.Fatal(err)
	}
	// Removing by the real path must undo what was installed by the alias.
	changed, err := removeClaudeUsage(context.Background(), real)
	if err != nil || !changed {
		t.Fatalf("remove %t %v", changed, err)
	}
	raw := readClaudeFixture(t, real, "settings.json")
	if strings.Contains(string(raw), claudeUsageMarker) || !strings.Contains(string(raw), "my-line") {
		t.Fatalf("not restored: %s", raw)
	}
	// And the reverse: installed by the real path, removed through the alias.
	mustWrite(t, filepath.Join(real, "settings.json"), []byte(settings))
	if _, err := installClaudeUsage(context.Background(), real, m, nil); err != nil {
		t.Fatal(err)
	}
	if changed, err := removeClaudeUsage(context.Background(), link); err != nil || !changed {
		t.Fatalf("remove via alias %t %v", changed, err)
	}
	if raw := readClaudeFixture(t, real, "settings.json"); strings.Contains(string(raw), claudeUsageMarker) {
		t.Fatalf("not removed: %s", raw)
	}
}

func TestSettingsWriteKeepsABackupAndAConflictLeavesNoManifest(t *testing.T) {
	m, _ := round3Fixture(t)
	profile := t.TempDir()
	path := filepath.Join(profile, "settings.json")
	original := `{"permissions":{"deny":["Bash(rm *)"]}}`
	mustWrite(t, path, []byte(original))
	if _, err := installClaudeUsage(context.Background(), profile, m, nil); err != nil {
		t.Fatal(err)
	}
	if got := readClaudeFixture(t, profile, "settings.json.minion-backup"); string(got) != original {
		t.Fatalf("backup %q", got)
	}
	// A settings file that changes while a fresh install is staged: nothing is
	// overwritten and no manifest stays behind.
	other := t.TempDir()
	mustWrite(t, filepath.Join(other, "settings.json"), []byte(original))
	passes := 0
	racing := func(context.Context, string) (ops.UsageAccount, bool) {
		passes++
		mustWrite(t, filepath.Join(other, "settings.json"), []byte(`{"changed":`+string(rune('0'+passes))+`}`))
		return ops.UsageAccount{}, false
	}
	_, err := installClaudeUsage(context.Background(), other, m, racing)
	if !errors.Is(err, errClaudeSettingsChanged) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, claudeUsageManifest)); err == nil {
		t.Fatal("a manifest was left behind")
	}
}

func TestExporterAttributesTheSubscriptionOnlyWithVerifiedEvidence(t *testing.T) {
	profile := t.TempDir()
	none := func(string) (string, bool) { return "", false }
	withKey := func(n string) (string, bool) { return "sk-x", n == "ANTHROPIC_API_KEY" }
	withHost := func(n string) (string, bool) { return "https://gw.example/v1", n == "ANTHROPIC_BASE_URL" }
	for name, c := range map[string]struct {
		lookup func(string) (string, bool)
		want   claudeAccessEvidence
	}{
		"no environment to read": {nil, claudeAccessUnverified},
		"nothing configured":     {none, claudeAccessSubscription},
		"api key in the env":     {withKey, claudeAccessAPI},
		"custom endpoint":        {withHost, claudeAccessCustomEndpoint},
	} {
		if got := claudeUsageAccess(c.lookup, profile); got != c.want {
			t.Errorf("%s: %v want %v", name, got, c.want)
		}
	}
	mustWrite(t, filepath.Join(profile, "settings.json"), []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"t"}}`))
	if got := claudeUsageAccess(none, profile); got != claudeAccessAPI {
		t.Errorf("a token in the profile settings: %v", got)
	}
	mustWrite(t, filepath.Join(profile, "settings.local.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example/v1"}}`))
	if got := claudeUsageAccess(none, profile); got != claudeAccessCustomEndpoint {
		t.Errorf("a base URL in the local profile settings: %v", got)
	}
}

func TestExporterRecordsAnAPISessionWithoutTheSubscription(t *testing.T) {
	st, en := claudeFixtureEnrollment(t, true)
	ctx := context.Background()
	grant, _ := st.UsageGrantID(ctx, en.WorkerID)
	m := claudeUsageIntegration{StateDir: st.Dir(), Grant: claudeUsageGrant(grant), Profile: "Synthetic subscription"}
	now := time.Now()
	status := []byte(`{"session_id":"s1","version":"2.1.320","cost":{"total_cost_usd":1.5}}`)
	hook := []byte(`{"session_id":"s1","hook_event_name":"Stop","prompt_id":"p1"}`)
	recordClaudeUsage(ctx, m, "status-line", "api-fixture", claudeAccessAPI, status, now)
	recordClaudeUsage(ctx, m, "Stop", "api-fixture", claudeAccessAPI, hook, now)
	start := now.Add(-time.Minute).UnixMilli()
	report := ops.UsageReport{ObservedAt: now.Add(time.Minute).UnixMilli(), Sessions: []ops.UsageSession{
		{ID: ops.UsageID("host-session", "x"), Name: "api-fixture", StartedAt: &start}, // the process has ended: no access evidence
	}}
	if err := st.NativeUsage(ctx, &report, 0); err != nil {
		t.Fatal(err)
	}
	if s := report.Sessions[0]; s.AuthMode != "api" || s.AccountID != "" {
		t.Fatalf("session %+v", s)
	}
	for _, e := range report.Events {
		if e.AccountID != "" {
			t.Fatalf("event carries an account: %+v", e)
		}
	}
}

func TestExporterRecordsClaudeProxyLimitsOnTheCustomEndpointAccount(t *testing.T) {
	st, en := claudeFixtureEnrollment(t, true)
	ctx := t.Context()
	grant, err := st.UsageGrantID(ctx, en.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	mustWrite(t, filepath.Join(profile, "settings.json"), []byte(`{"apiKeyHelper":"/bin/key","env":{"ANTHROPIC_BASE_URL":"https://proxy.example/v1"}}`))
	mustWrite(t, filepath.Join(profile, ".credentials.json"), []byte(`{"claudeAiOauth":{}}`))
	m := claudeUsageIntegration{StateDir: st.Dir(), Grant: claudeUsageGrant(grant), Profile: "Synthetic subscription"}
	now := time.Now()
	status := []byte(`{"session_id":"proxy-fixture","rate_limits":{"seven_day":{"used_percentage":25,"resets_at":1800000000}}}`)
	access := claudeUsageAccess(mapEnv(nil), profile)
	recordClaudeUsage(ctx, m, "status-line", "proxy-work", access, status, now)
	report := ops.UsageReport{ObservedAt: now.Add(time.Minute).UnixMilli(), Sessions: []ops.UsageSession{{
		ID: ops.UsageID("host-session", "proxy"), Name: "proxy-work", StartedAt: new(now.Add(-time.Minute).UnixMilli()), AuthMode: "subscription",
	}}}
	if err := st.NativeUsage(ctx, &report, 0); err != nil {
		t.Fatal(err)
	}
	s := report.Sessions[0]
	if s.AuthMode != "api" || !s.CustomEndpoint || s.AccountID == "" {
		t.Errorf("native access = %q, custom endpoint = %t, linked account = %t", s.AuthMode, s.CustomEndpoint, s.AccountID != "")
	}
	if len(s.Windows) != 1 || s.Windows[0].UsedPercent == nil || *s.Windows[0].UsedPercent != 25 {
		t.Errorf("native session lost its upstream window: %+v", s.Windows)
	}
	if len(report.Accounts) != 1 {
		t.Fatalf("got %d accounts, want one custom endpoint account", len(report.Accounts))
	}
	a := report.Accounts[0]
	if a.ID != s.AccountID || a.Label != "Custom endpoint (not told apart)" || !a.CustomEndpoint || a.AuthMode != "api" || a.SourceSessionID != s.ID || len(a.Windows) != 1 || a.Windows[0].UsedPercent == nil || *a.Windows[0].UsedPercent != 25 {
		t.Fatalf("wrong custom endpoint account: %+v", a)
	}
}
