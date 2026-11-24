package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
)

func TestCodexHookMergePreservesUserHooksAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	original := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"existing command","timeout":9}]}],"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"other command"}]}]}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	binary := filepath.Join(dir, "minion's executable")
	state := filepath.Join(dir, "worker state")
	if err := installCodexHooks(t.Context(), dir, binary, state); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Hooks["Stop"]) != 2 || !strings.Contains(string(config.Hooks["Stop"][0]), `"existing command"`) ||
		len(config.Hooks["PostToolUse"]) != 1 || !strings.Contains(string(config.Hooks["PostToolUse"][0]), `"other command"`) {
		t.Fatalf("user hooks were changed: %s", raw)
	}
	for _, event := range codexLifecycleHooks {
		want := codexHookCommand(binary, state)
		if len(config.Hooks[event.name]) == 0 {
			t.Fatalf("missing %s hook", event.name)
		}
		var group struct {
			Hooks []struct{ Command string } `json:"hooks"`
		}
		rules := config.Hooks[event.name]
		if err := json.Unmarshal(rules[len(rules)-1], &group); err != nil || len(group.Hooks) != 1 || group.Hooks[0].Command != want {
			t.Fatalf("missing Minion %s command: %s", event.name, rules)
		}
	}
	if next, err := mergeCodexHooks(raw, codexHookCommand(binary, state)); err != nil || next != nil {
		t.Fatalf("reinstall should leave profile untouched: %q, %v", next, err)
	}
	if _, err := mergeCodexHooks(raw, codexHookCommand(binary, state+"-other")); err == nil {
		t.Fatal("another Worker's trusted hook would be replaced")
	}
	backup, err := os.ReadFile(path + ".minion-backup")
	if err != nil || string(backup) != string(original) {
		t.Fatalf("missing copy of the original hook configuration: %v", err)
	}
}

func TestCodexExporterSavedOptOut(t *testing.T) {
	state := t.TempDir()
	file, err := config.LoadFile(filepath.Join(state, config.WorkerConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	file.Set(config.KeyUsageCodexExporter, "off")
	res := &config.Resolver{Role: config.ScopeWorker, File: file, LookupEnv: func(key string) (string, bool) {
		return "auto", key == "MINION_USAGE_CODEX_EXPORTER"
	}}
	if codexExporterEnabled(res) {
		t.Fatal("saved opt-out was overridden by the inherited environment")
	}
	dir := t.TempDir()
	autoInstallCodexExporter(t.Context(), res, state, func(string) (string, bool) { return dir, true })
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("automatic installer touched the opted-out profile: %v, %v", entries, err)
	}
	if err := file.Save(); err != nil {
		t.Fatal(err)
	}
	cmd := newCodexUsageExportCmd(res.LookupEnv)
	var out bytes.Buffer
	cmd.SetArgs([]string{state})
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"Stop","session_id":"private","turn_id":"private"}`))
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(t.Context()); err != nil || out.String() != "{}\n" {
		t.Fatalf("disabled Stop hook did not return neutral JSON: %q, %v", out.String(), err)
	}
}

func TestCodexHookVersion(t *testing.T) {
	for output, want := range map[string]bool{
		"codex-cli 0.171.0\n":    true,
		"codex-cli 0.172.1":      true,
		"codex-cli 0.171.0-rc.1": false,
		"codex-cli 0.170.0":      false,
		"codex 0.200.0":          false,
	} {
		if got := supportedCodexHooks(output); got != want {
			t.Errorf("%q: got %t, want %t", output, got, want)
		}
	}
}

func TestCodexExporterRequiresAcceptedUsage(t *testing.T) {
	if !codexHookPlatform() {
		t.Skip("the Codex installer needs a supported native process binding")
	}
	dir := t.TempDir()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\necho 'codex-cli 0.171.0'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	lookup := func(key string) (string, bool) { return dir, key == "CODEX_HOME" }
	resolver := &config.Resolver{Role: config.ScopeWorker, LookupEnv: lookup}
	pending, _ := claudeFixtureEnrollment(t, false)
	if !autoInstallCodexExporter(t.Context(), resolver, pending.Dir(), lookup) {
		t.Fatal("the exporter should retry while consent is pending")
	}
	hooks := filepath.Join(dir, "hooks.json")
	if _, err := os.Stat(hooks); !os.IsNotExist(err) {
		t.Fatalf("hook config changed before usage acceptance: %v", err)
	}
	accepted, _ := claudeFixtureEnrollment(t, true)
	if autoInstallCodexExporter(t.Context(), resolver, accepted.Dir(), lookup) {
		t.Fatal("accepted enrollment should no longer be pending")
	}
	if _, err := os.Stat(hooks); err != nil {
		t.Fatalf("missing hook config after usage acceptance: %v", err)
	}
}

func TestCodexLifecycleMetadata(t *testing.T) {
	at := time.Now()
	start, ok := parseCodexLifecycle(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"thread","turn_id":"turn","prompt":"private prompt","transcript_path":"private path"}`), "grant", at)
	if !ok || start.Kind != "turn_start" || start.NativeID == "thread" || start.Entity == "turn" || start.At != at {
		t.Fatalf("unexpected start: %+v, %v", start, ok)
	}
	for _, name := range []string{"Stop", "Interrupt", "PreModelRequest"} {
		payload := `{"hook_event_name":"` + name + `","session_id":"thread","turn_id":"turn","last_assistant_message":"private reply"}`
		event, ok := parseCodexLifecycle(strings.NewReader(payload), "grant", at)
		want := "turn_end"
		if name == "PreModelRequest" {
			want = "turn_start"
		}
		if !ok || event.Kind != want || event.NativeID != start.NativeID || event.Entity != start.Entity {
			t.Fatalf("%s failed to correlate the turn: %+v, %v", name, event, ok)
		}
		other, ok := parseCodexLifecycle(strings.NewReader(payload), "other-grant", at)
		if !ok || other.NativeID == event.NativeID || other.Entity == event.Entity {
			t.Fatalf("%s reused IDs across grants", name)
		}
	}
	for _, rejected := range []string{
		`{"hook_event_name":"PreModelRequest","session_id":"thread","turn_id":"child","agent_id":"agent"}`,
		`{"hook_event_name":"SessionStart","session_id":"thread","source":"compact"}`,
		`{"hook_event_name":"Stop","session_id":"thread"}`,
		`{"hook_event_name":"Unknown","session_id":"thread"}`,
		`{"hook_event_name":"SessionEnd","session_id":"thread"}{}`,
	} {
		if _, ok := parseCodexLifecycle(strings.NewReader(rejected), "grant", at); ok {
			t.Fatalf("unexpected observation: %s", rejected)
		}
	}
}
