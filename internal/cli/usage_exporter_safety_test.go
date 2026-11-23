package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func exporterProfile(t *testing.T, settings string) (home, profile, state string, none func(string) (string, bool)) {
	t.Helper()
	home, state, _ = autoInstallFixture(t, true, nil)
	profile = filepath.Join(home, ".claude")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(profile, "settings.json"), []byte(settings))
	return home, profile, state, func(string) (string, bool) { return "", false }
}

func TestAutoInstallFailsClosedOnAPolicyError(t *testing.T) {
	_, state, res := autoInstallFixture(t, true, map[string]string{"MINION_USAGE_CLAUDE_EXPORTER": "of"})
	_, profile, _, none := exporterProfile(t, `{"permissions":{}}`)
	autoInstallClaudeExporter(context.Background(), res, state, none)
	if _, err := os.Stat(filepath.Join(profile, claudeUsageManifest)); err == nil || agent.ClaudeExporterIssue() != ops.UsageExporterConflict {
		t.Fatalf("an invalid policy value enabled the install (issue %q)", agent.ClaudeExporterIssue())
	}
	autoInstallClaudeExporter(context.Background(), nil, state, none)
	if _, err := os.Stat(filepath.Join(profile, claudeUsageManifest)); err == nil {
		t.Fatal("an unreadable policy enabled the install")
	}
}

func TestAutoInstallRespectsManagedDropIns(t *testing.T) {
	_, profile, state, none := exporterProfile(t, `{"permissions":{}}`)
	_, _, res := autoInstallFixture(t, true, nil) // fresh fixture resets the managed file path
	managed := agent.ClaudeManagedSettingsFile
	if err := os.MkdirAll(filepath.Join(filepath.Dir(managed), "managed-settings.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(filepath.Dir(managed), "managed-settings.d", "50-hooks.json"), []byte(`{"allowManagedHooksOnly":true}`))
	autoInstallClaudeExporter(context.Background(), res, state, none)
	if _, err := os.Stat(filepath.Join(profile, claudeUsageManifest)); err == nil || agent.ClaudeExporterIssue() != ops.UsageExporterOverridden {
		t.Fatalf("a managed drop-in did not stop the install (issue %q)", agent.ClaudeExporterIssue())
	}
	// The explicit install respects it too.
	_, err := installClaudeUsage(context.Background(), profile, claudeUsageIntegration{Version: 1, Binary: "/bin/minion", StateDir: state, Shell: "/bin/sh"}, nil)
	if !errors.Is(err, errClaudeManaged) {
		t.Fatalf("explicit install: %v", err)
	}
}

func TestInstallKeepsAConcurrentClaudeSaveAndReportsAConflict(t *testing.T) {
	_, profile, state, _ := exporterProfile(t, `{"permissions":{}}`)
	path := filepath.Join(profile, "settings.json")
	integration := claudeUsageIntegration{Version: 1, Binary: "/bin/minion", StateDir: state, Shell: "/bin/sh"}
	// Claude saves once while the installer works: the retry merges on top of it.
	saved := false
	discover := func(context.Context, string) (ops.UsageAccount, bool) {
		if !saved {
			saved = true
			mustWrite(t, path, []byte(`{"permissions":{"deny":["Bash(rm *)"]}}`))
		}
		return ops.UsageAccount{}, false
	}
	if _, err := installClaudeUsage(context.Background(), profile, integration, discover); err != nil {
		t.Fatal(err)
	}
	raw := readClaudeFixture(t, profile, "settings.json")
	if !bytes.Contains(raw, []byte("Bash(rm *)")) || !bytes.Contains(raw, []byte(claudeUsageMarker)) {
		t.Fatalf("the concurrent save was lost or the install missing: %s", raw)
	}
	// A save on every attempt: a conflict is reported and the file stays Claude's.
	_, profile2, state2, _ := exporterProfile(t, `{"permissions":{}}`)
	path2 := filepath.Join(profile2, "settings.json")
	n := 0
	always := func(context.Context, string) (ops.UsageAccount, bool) {
		n++
		mustWrite(t, path2, []byte(`{"permissions":{"n":`+string(rune('0'+n))+`}}`))
		return ops.UsageAccount{}, false
	}
	integration.StateDir = state2
	_, err := installClaudeUsage(context.Background(), profile2, integration, always)
	if !errors.Is(err, errClaudeSettingsChanged) {
		t.Fatalf("err %v", err)
	}
	if got := readClaudeFixture(t, profile2, "settings.json"); bytes.Contains(got, []byte(claudeUsageMarker)) || !bytes.Contains(got, []byte(`"n":2`)) {
		t.Fatalf("the installer overwrote a Claude save: %s", got)
	}
}

func TestRemoveDeletesOnlyMinionsOwnHooks(t *testing.T) {
	_, profile, state, _ := exporterProfile(t, `{}`)
	integration := claudeUsageIntegration{Version: 1, Binary: "/bin/minion", StateDir: state, Shell: "/bin/sh"}
	if _, err := installClaudeUsage(context.Background(), profile, integration, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profile, "settings.json")
	var s map[string]json.RawMessage
	if err := json.Unmarshal(readClaudeFixture(t, profile, "settings.json"), &s); err != nil {
		t.Fatal(err)
	}
	var hooks map[string][]json.RawMessage
	if err := json.Unmarshal(s["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	user1 := json.RawMessage(`{"hooks":[{"type":"command","command":"echo _claude-export mention"}]}`)
	user2 := json.RawMessage(`{"matcher":"Bash","hooks":[{"type":"command","command":"/bin/minion","args":["worker","config","usage","integrate","_claude-export","Stop","/other/manifest"]}]}`)
	hooks["Notification"] = []json.RawMessage{user1}
	hooks["Stop"] = append(hooks["Stop"], user2)
	s["hooks"], _ = json.Marshal(hooks)
	out, _ := json.Marshal(s)
	mustWrite(t, path, out)
	if changed, err := removeClaudeUsage(context.Background(), profile); err != nil || !changed {
		t.Fatalf("remove %v %v", changed, err)
	}
	got := string(readClaudeFixture(t, profile, "settings.json"))
	if !strings.Contains(got, "echo _claude-export mention") || !strings.Contains(got, "/other/manifest") {
		t.Fatalf("an unrelated hook was removed: %s", got)
	}
	if strings.Contains(got, filepath.Join(profile, claudeUsageManifest)) {
		t.Fatalf("Minion's own hook remains: %s", got)
	}
}

func TestInstallStoresTheVerifiedLoginLabelForPassiveReports(t *testing.T) {
	_, profile, state, _ := exporterProfile(t, `{}`)
	integration := claudeUsageIntegration{Version: 1, Binary: "/bin/minion", StateDir: state, Shell: "/bin/sh"}
	id := ops.UsageID("subscription", "Anthropic", "me")
	discover := func(context.Context, string) (ops.UsageAccount, bool) {
		return ops.UsageAccount{ID: id, Provider: "Anthropic", AuthMode: "subscription", Label: "someone@example.org / abc123"}, true
	}
	if _, err := installClaudeUsage(context.Background(), profile, integration, discover); err != nil {
		t.Fatal(err)
	}
	m, err := readClaudeUsageManifest(filepath.Join(profile, claudeUsageManifest))
	if err != nil || m == nil || m.Label != "someone@example.org / abc123" {
		t.Fatalf("manifest %+v %v", m, err)
	}
	// A second pass with the same login changes nothing and is not a rebinding.
	if changed, err := installClaudeUsage(context.Background(), profile, integration, discover); err != nil || changed {
		t.Fatalf("repeat: %v %v", changed, err)
	}
}
