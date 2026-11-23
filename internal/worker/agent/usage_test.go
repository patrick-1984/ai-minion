package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

type usageHostFixture struct{ sessions []term.Session }

func (h usageHostFixture) ListSessions(context.Context) ([]term.Session, error) {
	return h.sessions, nil
}

func TestUsageProjectionDropsTaskTextAndCommandArguments(t *testing.T) {
	host := usageHostFixture{[]term.Session{{Name: "private-build", Task: "PRIVATE sample task payload", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Panes: []term.Pane{{Active: true, Ref: term.PaneRef{Command: "codex -- PRIVATE invocation", PID: 791}}}}}}
	r, err := usageSnapshot(context.Background(), host, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sessions) != 1 || r.Sessions[0].Name != "private-build" || r.Sessions[0].StartedAt == nil || r.Sessions[0].InputTokens != nil || r.Sessions[0].AuthMode != "unknown" {
		t.Fatalf("unexpected projection: %+v", r)
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"PRIVATE", "791", "command", "screen", "transcript", "prompt"} {
		if strings.Contains(string(body), field) {
			t.Fatalf("extra field/content: %s", field)
		}
	}
}

type usageProcs []agentdetect.Proc

func (p usageProcs) Processes(context.Context) ([]agentdetect.Proc, error) { return p, nil }

func TestUsageSnapshotFindsWrappedAgentInProcessTree(t *testing.T) {
	old := usageDetector
	t.Cleanup(func() { usageDetector = old })
	usageDetector = agentdetect.New(agentdetect.WithProcessSource(usageProcs{
		{PID: 100, PPID: 1, PGRP: 100, TPGID: 300, TTY: "pts/1", Args: []string{"-bash"}},
		{PID: 200, PPID: 100, PGRP: 300, TPGID: 300, TTY: "pts/1", Args: []string{"bash", "-c", ": wb-restored claude; claude --resume x; exec bash -l"}},
		{PID: 300, PPID: 200, PGRP: 300, TPGID: 300, TTY: "pts/1", Args: []string{"claude", "--resume", "x"}},
		{PID: 400, PPID: 1, PGRP: 400, TPGID: 500, TTY: "pts/2", Args: []string{"-bash"}},
	}))
	pane := func(pid int) []term.Pane {
		return []term.Pane{{Active: true, Ref: term.PaneRef{Command: "bash", PID: pid}}}
	}
	host := usageHostFixture{[]term.Session{{Name: "wb-1", Panes: pane(100)}, {Name: "plain", Panes: pane(400)}}}
	r, err := usageSnapshot(context.Background(), host, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Sessions[0]; got.Harness != "claude" || got.Provider != "Anthropic" {
		t.Fatalf("wrapped claude not detected: %+v", got)
	}
	if got := r.Sessions[1]; got.Harness != "" || got.Provider != "" {
		t.Fatalf("plain shell got a harness: %+v", got)
	}
}

func TestClaudeExporterState(t *testing.T) {
	write := func(body string) string {
		dir := t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	line := `{"statusLine":{"type":"command","command":"'/bin/minion' 'worker' 'config' 'usage' 'integrate' '_claude-export' 'status-line' 'm'"}`
	for name, tc := range map[string]struct{ body, want string }{
		"no file":    {"", ops.UsageExporterAbsent},
		"other line": {`{"statusLine":{"type":"command","command":"my-line"}}`, ops.UsageExporterAbsent},
		"installed":  {line + `}`, ops.UsageExporterInstalled},
		"overridden": {line + `,"disableAllHooks":true}`, ops.UsageExporterOverridden},
		"broken":     {`{`, ops.UsageExporterAbsent},
	} {
		if got := claudeExporterStateIn(write(tc.body)); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

func TestManagedPolicyDropInsAndUnreadablePolicyOverride(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "managed-settings.json")
	if ClaudeManagedOverrides(file) {
		t.Fatal("no managed settings must not override")
	}
	if err := os.MkdirAll(filepath.Join(dir, "managed-settings.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	drop := filepath.Join(dir, "managed-settings.d", "10-hooks.json")
	if err := os.WriteFile(drop, []byte(`{"permissions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ClaudeManagedOverrides(file) {
		t.Fatal("a drop-in without hook policy overrides")
	}
	if err := os.WriteFile(drop, []byte(`{"allowManagedHooksOnly":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ClaudeManagedOverrides(file) {
		t.Fatal("a drop-in with allowManagedHooksOnly is ignored")
	}
	if err := os.WriteFile(drop, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ClaudeManagedOverrides(file) {
		t.Fatal("an unparsable managed policy must fail closed")
	}
}

func TestClaudeExporterStateReportsAManagedOverrideAfterInstall(t *testing.T) {
	old := ClaudeManagedSettingsFile
	t.Cleanup(func() { ClaudeManagedSettingsFile = old })
	ClaudeManagedSettingsFile = filepath.Join(t.TempDir(), "managed-settings.json")
	dir := t.TempDir()
	line := `{"statusLine":{"type":"command","command":"'/bin/minion' '_claude-export' 'status-line' 'm'"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeExporterStateIn(dir); got != ops.UsageExporterInstalled {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(ClaudeManagedSettingsFile, []byte(`{"disableAllHooks":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := claudeExporterStateIn(dir); got != ops.UsageExporterOverridden {
		t.Fatalf("installed marker hides a managed override: %q", got)
	}
}
