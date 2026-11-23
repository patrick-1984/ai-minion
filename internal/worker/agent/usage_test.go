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
