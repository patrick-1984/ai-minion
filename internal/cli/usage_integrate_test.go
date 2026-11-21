package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func claudeFixtureEnrollment(t *testing.T, accept bool) (*store.Store, store.Enrollment) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	en := store.Enrollment{HubPub: key.Public(), WorkerID: ops.NewUUIDv7(), Owner: "fixture-owner"}
	if err := st.SaveEnrollment(context.Background(), en); err != nil {
		t.Fatal(err)
	}
	if accept {
		if err := st.AcceptUsageRegistration(context.Background(), en.HubPub, en.WorkerID, en.Owner); err != nil {
			t.Fatal(err)
		}
	}
	return st, en
}

func readClaudeFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUsageIntegrateClaudePreservesProfileSettingsAndIsIdempotent(t *testing.T) {
	st, _ := claudeFixtureEnrollment(t, true)
	target := filepath.Join(t.TempDir(), "personal profile's settings")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alternate := t.TempDir()
	settings := `{
	  "statusLine": {"type": "command", "command": "printf 'original status: '; cat", "padding": 7, "refreshInterval": 20},
	  "hooks": {
	    "Stop": [{"hooks": [{"type": "command", "command": "existing-stop", "timeout": 3}]}],
	    "PostToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "keep-post-tool"}]}]
	  },
	  "permissions": {"deny": ["Bash(rm *)"]}, "autoUpdatesChannel": "latest"
	}`
	path := filepath.Join(target, "settings.json")
	mustWrite(t, path, []byte(settings))
	deps := testConfigDeps(map[string]string{"CLAUDE_CONFIG_DIR": alternate})
	args := []string{"usage", "integrate", "claude", "--claude-config-dir", target, "--account", ops.UsageID("acct", "anthropic")}
	out, _, err := configCmd(t, deps, st.Dir(), args...)
	if err != nil || !strings.Contains(out, target) {
		t.Fatalf("integrate returned %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(alternate, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("integration changed the environment's different profile: %v", err)
	}
	current := readClaudeFixture(t, target, "settings.json")
	m, err := readClaudeUsageManifest(filepath.Join(target, claudeUsageManifest))
	if err != nil || m == nil || m.PreviousLine != "printf 'original status: '; cat" || m.StateDir != st.Dir() || m.Account != args[len(args)-1] {
		t.Fatalf("manifest invalid: %+v, %v", m, err)
	}
	var got struct {
		Status map[string]any               `json:"statusLine"`
		Hooks  map[string][]json.RawMessage `json:"hooks"`
		Perms  map[string][]string          `json:"permissions"`
		Auto   string                       `json:"autoUpdatesChannel"`
	}
	if err := json.Unmarshal(current, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status["padding"] != float64(7) || got.Status["refreshInterval"] != float64(20) ||
		got.Status["command"] != m.statusCommand(filepath.Join(target, claudeUsageManifest)) || got.Auto != "latest" ||
		!slices.Equal(got.Perms["deny"], []string{"Bash(rm *)"}) {
		t.Fatalf("existing profile settings changed unexpectedly: %+v", got)
	}
	if len(got.Hooks["Stop"]) != 2 || !bytes.Contains(got.Hooks["Stop"][0], []byte("existing-stop")) ||
		len(got.Hooks["PostToolUse"]) != 1 || !bytes.Contains(got.Hooks["PostToolUse"][0], []byte("keep-post-tool")) {
		t.Fatalf("existing hooks changed: %+v", got.Hooks)
	}
	for _, event := range claudeUsageEvents {
		rules := got.Hooks[event]
		if len(rules) == 0 {
			t.Fatalf("missing hook for %s", event)
		}
		found, err := claudeUsageFindHook(rules, claudeUsageHook{Type: "command", Command: m.Binary, Args: m.args(event, filepath.Join(target, claudeUsageManifest)), Async: event != "SessionEnd"})
		if err != nil || !found {
			t.Fatalf("managed %s hook = %t, %v", event, found, err)
		}
	}
	out, _, err = configCmd(t, deps, st.Dir(), args...)
	if err != nil || !strings.Contains(out, "already configured") || !bytes.Equal(current, readClaudeFixture(t, target, "settings.json")) {
		t.Fatalf("repeat install modified the profile: %q, %v", out, err)
	}

	// A different subscription selection must not replace the private binding
	// or rewrite the installed status line and hooks.
	otherArgs := slices.Clone(args)
	otherArgs[len(otherArgs)-1] = ops.UsageID("acct", "other")
	_, _, err = configCmd(t, deps, st.Dir(), otherArgs...)
	if err == nil || !bytes.Equal(current, readClaudeFixture(t, target, "settings.json")) {
		t.Fatal("a conflicting account selection modified the profile")
	}
}

func TestUsageIntegrateClaudeRefusesUnsafeSettingsAndMissingConsent(t *testing.T) {
	st, en := claudeFixtureEnrollment(t, false)
	profile := filepath.Join(t.TempDir(), "unused")
	args := []string{"usage", "integrate", "claude", "--claude-config-dir", profile, "--subscription-profile", "Fixture"}
	_, _, err := configCmd(t, testConfigDeps(nil), st.Dir(), args...)
	if err == nil || !strings.Contains(err.Error(), "--accept") {
		t.Fatalf("setup without monitoring acceptance: %v", err)
	}
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup changed an unaccepted profile: %v", err)
	}
	if err := st.AcceptUsageRegistration(context.Background(), en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"disableAllHooks":true}`,
		`{"statusLine":{"type":"unsupported","command":"foo"}}`,
		`{"statusLine":{}}`,
		`{"hooks":{"Stop":{"invalid":[]}}}`,
		`{"statusLine":{"type":"command","command":"old _claude-export"} }`,
		`{"unknown":1,"unknown":2}`,
		`{"hooks": {}, }`,
	} {
		t.Run(bad, func(t *testing.T) {
			profile := t.TempDir()
			path := filepath.Join(profile, "settings.json")
			mustWrite(t, path, []byte(bad))
			_, _, err := configCmd(t, testConfigDeps(nil), st.Dir(),
				"usage", "integrate", "claude", "--claude-config-dir", profile, "--subscription-profile", "Fixture")
			if err == nil || !bytes.Equal(readClaudeFixture(t, profile, "settings.json"), []byte(bad)) {
				t.Fatalf("profile conflict was overwritten: %v", err)
			}
			if _, err := os.Stat(filepath.Join(profile, claudeUsageManifest)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("conflicting settings created a manifest: %v", err)
			}
		})
	}
}

func TestUsageIntegrateClaudeNativeDiscoveryUsesSelectedProfile(t *testing.T) {
	st, _ := claudeFixtureEnrollment(t, true)
	profile := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell, powerShell, err := claudeUsageShell(os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	id := ops.UsageID("acct", "fixture")
	m := claudeUsageIntegration{Version: 1, Binary: binary, StateDir: st.Dir(), Shell: shell, PowerShell: powerShell}
	discover := func(_ context.Context, got string) (ops.UsageAccount, bool) {
		if got != profile {
			t.Fatalf("native discovery pointed at %q instead of selected fixture profile", got)
		}
		return ops.UsageAccount{ID: id, Provider: "Anthropic", AuthMode: "subscription", Plan: "max"}, true
	}
	changed, err := installClaudeUsage(context.Background(), profile, m, discover)
	if err != nil || !changed {
		t.Fatalf("integration returned %t, %v", changed, err)
	}
	saved, err := readClaudeUsageManifest(filepath.Join(profile, claudeUsageManifest))
	if err != nil || saved == nil || saved.Account != id || saved.Plan != "max" {
		t.Fatalf("native account not bound: %+v, %v", saved, err)
	}
}

func TestUsageIntegrateClaudeStatusLinePreservesInputOutputAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX shell")
	}
	ctx := context.Background()
	m := claudeUsageIntegration{Shell: "/bin/sh", PreviousLine: "printf '[original]'; cat; printf 'original error' >&2; exit 7"}
	for _, size := range []int{100, maxClaudeUsagePayload + 203} {
		input := bytes.Repeat([]byte("a"), size)
		var stdout, stderr bytes.Buffer
		captured := make(chan []byte, 1)
		err := dispatchClaudeUsage(ctx, m, "status-line", bytes.NewReader(input), &stdout, &stderr,
			func(_ context.Context, data []byte, _ time.Time) { captured <- data })
		var exit *exitCodeError
		if !errors.As(err, &exit) || exit.code != 7 || !bytes.Equal(stdout.Bytes(), append([]byte("[original]"), input...)) ||
			stderr.String() != "original error" {
			t.Fatalf("wrapped exit/output differs for %d input bytes: %v, %d stdout bytes, %q stderr", size, err, stdout.Len(), stderr.String())
		}
		if size > maxClaudeUsagePayload {
			if len(captured) != 0 {
				t.Fatal("the wrapper exported a truncated status sample")
			}
		} else if len(captured) != 1 || !bytes.Equal(<-captured, input) {
			t.Fatal("the wrapper failed to observe the original input")
		}
	}
}

func TestUsageIntegrateClaudeExportProjectsOnlyMetadata(t *testing.T) {
	st, en := claudeFixtureEnrollment(t, true)
	ctx := context.Background()
	grant, err := st.UsageGrantID(ctx, en.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	m := claudeUsageIntegration{StateDir: st.Dir(), Grant: claudeUsageGrant(grant), Profile: "Synthetic subscription"}
	status := []byte(`{
	  "session_id":"test-session","version":"2.1.320","model":{"id":"synthetic-model"},
	  "workspace":{"current_dir":"/synthetic/folder"},
	  "cost":{"total_cost_usd":0.125,"total_duration_ms":3500},
	  "context_window":{"total_input_tokens":12,"context_window_size":99},
	  "prompt_cache":{"cache_write_tokens":42},
	  "rate_limits":{"five_hour":{"used_percentage":25,"resets_at":1800000000}},
	  "transcript_path":"/private/transcript","credential":"synthetic-password"
	}`)
	promptHook := []byte(`{
	  "session_id":"test-session","hook_event_name":"UserPromptSubmit","prompt_id":"test-prompt-id",
	  "cwd":"/synthetic/folder","prompt":"synthetic private prompt","transcript_path":"/private/transcript"
	}`)
	now := time.Now()
	recordClaudeUsage(ctx, m, "status-line", "unshared-fixture", status, now)
	recordClaudeUsage(ctx, m, "UserPromptSubmit", "unshared-fixture", promptHook, now)
	recordClaudeUsage(ctx, m, "UserPromptSubmit", "unshared-fixture", promptHook, now)
	recordClaudeUsage(ctx, m, "Stop", "unshared-fixture", promptHook, now)
	subagent := []byte(`{"session_id":"test-session","hook_event_name":"SubagentStop","agent_id":"fixture-agent","last_assistant_message":"synthetic private subagent reply"}`)
	recordClaudeUsage(ctx, m, "SubagentStop", "unshared-fixture", subagent, now)
	modelSwitch := []byte(`{"session_id":"test-session","hook_event_name":"PostModelSwitch","model":"synthetic-model"}`)
	recordClaudeUsage(ctx, m, "PostModelSwitch", "unshared-fixture", modelSwitch, now)
	start := now.Add(-time.Minute).UnixMilli()
	report := ops.UsageReport{ObservedAt: now.Add(time.Minute).UnixMilli(), Sessions: []ops.UsageSession{
		{ID: ops.UsageID("host-session", "test"), Name: "unshared-fixture", StartedAt: &start},
	}}
	if err := st.NativeUsage(ctx, &report, 0); err != nil {
		t.Fatal(err)
	}
	s := report.Sessions[0]
	if s.Model != "synthetic-model" || s.Folder != "/synthetic/folder" || s.EstimatedCostUSD == nil || *s.EstimatedCostUSD != 0.125 ||
		s.ContextTokens == nil || *s.ContextTokens != 12 || s.DurationMS == nil || *s.DurationMS != 3500 ||
		s.CacheWriteTokens == nil || *s.CacheWriteTokens != 42 ||
		len(report.Events) != 3 || report.Events[0].Kind != "turn_started" || report.Events[1].Kind != "subagent_finished" ||
		report.Events[2].Kind != "model_changed" || len(report.Accounts) != 1 ||
		len(report.Accounts[0].Windows) != 1 || report.Accounts[0].Windows[0].UsedPercent == nil ||
		*report.Accounts[0].Windows[0].UsedPercent != 25 {
		t.Fatalf("native sample missing expected telemetry: %+v", report)
	}
	var sessionBody, eventBody string
	if err := st.DB().QueryRow("SELECT body FROM usage_native").Scan(&sessionBody); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow("SELECT group_concat(body, char(10)) FROM usage_native_events").Scan(&eventBody); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic private prompt", "synthetic-password", "/private/transcript", "test-prompt-id", "synthetic private subagent reply", "fixture-agent"} {
		if strings.Contains(sessionBody, secret) || strings.Contains(eventBody, secret) {
			t.Fatalf("native content persisted: %q", secret)
		}
	}
	en.Owner = "other-owner"
	if err := st.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	recordClaudeUsage(ctx, m, "status-line", "unshared-fixture", status, time.Now())
	var count int
	if err := st.DB().QueryRow("SELECT count(*) FROM usage_native").Scan(&count); err != nil || count != 0 {
		t.Fatalf("an old binding restored native metrics: %d, %v", count, err)
	}
}

func TestUsageIntegrateClaudeSilentHook(t *testing.T) {
	called := false
	var out bytes.Buffer
	err := dispatchClaudeUsage(context.Background(), claudeUsageIntegration{}, "SessionEnd", strings.NewReader(`{"hook_event_name":"SessionEnd"}`), &out, io.Discard,
		func(_ context.Context, _ []byte, _ time.Time) { called = true })
	if err != nil || !called || out.Len() != 0 {
		t.Fatalf("passive hook returned a command response: %q, %t, %v", out.String(), called, err)
	}
}

func TestUsageIntegrateClaudeStatusCommandQuotesExecutableAndManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX shell")
	}
	dir := filepath.Join(t.TempDir(), "profile with apostrophe's")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "fake minion's binary")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := claudeUsageIntegration{Binary: exe, Shell: "/bin/sh"}
	manifest := filepath.Join(dir, claudeUsageManifest)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", m.statusCommand(manifest))
	got, err := cmd.Output()
	if err != nil || !slices.Equal(strings.Split(strings.TrimSuffix(string(got), "\n"), "\n"), m.args("status-line", manifest)) {
		t.Fatalf("configured status command arguments: %q, %v", got, err)
	}
}

func TestUsageIntegrateClaudeGeneratedHookRoutesOnlyToSelectedProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a synthetic POSIX tmux shim")
	}
	st, _ := claudeFixtureEnrollment(t, true)
	profile, tmpBin := t.TempDir(), t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := claudeUsageIntegration{Version: 1, Binary: exe, StateDir: st.Dir(), Shell: "/bin/sh", Profile: "Fixture"}
	manifest := filepath.Join(profile, claudeUsageManifest)
	if _, err := installClaudeUsage(t.Context(), profile, m, nil); err != nil {
		t.Fatal(err)
	}
	// This is the only tmux executable the exporter can find in this test.
	// It answers the read-only query without touching any tmux server.
	if err := os.WriteFile(filepath.Join(tmpBin, "tmux"), []byte("#!/bin/sh\n[ \"$*\" = 'display-message -p -t %7 #{session_name}' ] || exit 2\nprintf 'private-fixture\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpBin)
	run := func(configDir string) {
		t.Helper()
		var output bytes.Buffer
		cmd := newClaudeUsageExportCmd(mapEnv(map[string]string{"TMUX_PANE": "%7", "CLAUDE_CONFIG_DIR": configDir}))
		cmd.SetArgs([]string{"SessionStart", manifest})
		cmd.SetIn(strings.NewReader(`{"session_id":"fixture-native","hook_event_name":"SessionStart","cwd":"/fixture"}`))
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		if err := cmd.ExecuteContext(t.Context()); err != nil || output.Len() > 0 {
			t.Fatalf("generated hook result: %q, %v", output.String(), err)
		}
	}
	run(t.TempDir())
	count := func(want int) {
		t.Helper()
		var got int
		if err := st.DB().QueryRow("SELECT count(*) FROM usage_native_events").Scan(&got); err != nil || got != want {
			t.Fatalf("profile-scoped hook events = %d, %v, want %d", got, err, want)
		}
	}
	count(0)
	run(profile)
	count(1)
}
