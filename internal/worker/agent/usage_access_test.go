package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func fakeEnv(t *testing.T, home string, envs map[int][]string) {
	t.Helper()
	old := processAccess
	t.Cleanup(func() { processAccess = old })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	processAccess = func(pid int, _ bool) ([]string, claudeSrc, bool) {
		e, ok := envs[pid]
		return filterProcessEnv([]byte(strings.Join(e, "\x00"))), claudeSrc{}, ok
	}
}

func TestDetectSessionAccess(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeEnv(t, home, map[int][]string{
		1: {"PATH=/bin", "ANTHROPIC_API_KEY=sk-ant-SECRET"},
		2: {"ANTHROPIC_AUTH_TOKEN=SECRET", "ANTHROPIC_BASE_URL=https://cpa.example:8443/v1/path"},
		3: {"ANTHROPIC_BASE_URL=https://api.anthropic.com", "ANTHROPIC_API_KEY=x"},
		4: {"PATH=/bin"},
		5: {"OPENAI_API_KEY=sk-SECRET", "OPENAI_BASE_URL=http://gw.internal/v1"},
		6: {"OPENAI_API_KEY=sk-SECRET"},
	})
	for name, tc := range map[string]struct {
		harness string
		pid     int
		want    sessionAccess
	}{
		"claude key":       {"claude", 1, sessionAccess{Mode: "api", Kind: accessAPIKey}},
		"claude endpoint":  {"claude", 2, sessionAccess{Mode: "api", Kind: accessCustomEndpoint}},
		"vendor host":      {"claude", 3, sessionAccess{Mode: "api", Kind: accessAPIKey}},
		"claude no signal": {"claude", 4, sessionAccess{Mode: "unknown"}},
		"codex endpoint":   {"codex", 5, sessionAccess{Mode: "api", Kind: accessCustomEndpoint}},
		"codex key":        {"codex", 6, sessionAccess{Mode: "api", Kind: accessAPIKey}},
		"unreadable env":   {"claude", 99, sessionAccess{Mode: "unknown"}},
		"other harness":    {"gemini", 1, sessionAccess{Mode: "unknown"}},
	} {
		if got := detectSessionAccess(tc.harness, tc.pid); got != tc.want {
			t.Errorf("%s: got %+v want %+v", name, got, tc.want)
		}
	}
	// Signed-in profile and settings file keys (names only are used).
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := detectSessionAccess("claude", 4); got.Mode != "subscription" {
		t.Errorf("credentials file: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://gw.example/x"},"apiKeyHelper":"/bin/key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := detectSessionAccess("claude", 4); got != (sessionAccess{Mode: "api", Kind: accessCustomEndpoint}) {
		t.Errorf("settings env: %+v", got)
	}
}

func TestUsageSnapshotReportsAccessWithoutSecrets(t *testing.T) {
	fakeEnv(t, t.TempDir(), map[int][]string{300: {"ANTHROPIC_API_KEY=sk-ant-VERYSECRET", "ANTHROPIC_BASE_URL=https://cpa.example/v1"}})
	old := usageDetector
	t.Cleanup(func() { usageDetector = old })
	usageDetector = agentdetect.New(agentdetect.WithProcessSource(usageProcs{
		{PID: 100, PPID: 1, PGRP: 100, TPGID: 300, TTY: "pts/1", Args: []string{"-bash"}},
		{PID: 300, PPID: 100, PGRP: 300, TPGID: 300, TTY: "pts/1", Args: []string{"claude"}},
	}))
	host := usageHostFixture{[]term.Session{{Name: "wb-1", Panes: []term.Pane{{Active: true, Ref: term.PaneRef{Command: "bash", PID: 100}}}}}}
	r, err := usageSnapshot(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Sessions[0]
	if s.AuthMode != "api" || s.Endpoint != "" || !s.CustomEndpoint || !r.Accounts[0].CustomEndpoint || s.AccountID == "" || len(r.Accounts) != 1 || r.Accounts[0].ID != s.AccountID || r.Accounts[0].Label != "Anthropic custom endpoint (not told apart)" {
		t.Fatalf("session %+v accounts %+v", s, r.Accounts)
	}
	if body := strings.ToLower(string(mustJSON(t, r))); strings.Contains(body, "verysecret") || strings.Contains(body, "/v1") || strings.Contains(body, "cpa.example") {
		t.Fatalf("secret or path leaked: %s", body)
	}
}

func TestUsageSnapshotClaudeSettingsOverrideOAuthCredentials(t *testing.T) {
	for _, name := range []string{"settings.json", "settings.local.json", "--settings"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			profile := filepath.Join(home, ".claude")
			if err := os.Mkdir(profile, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(profile, name)
			args := []string{"claude"}
			if name == "--settings" {
				path = filepath.Join(home, "worker-settings.json")
				args = append(args, "--settings", path)
			}
			if err := os.WriteFile(path, []byte(`{"apiKeyHelper":"/bin/key","env":{"ANTHROPIC_BASE_URL":"https://proxy.example/v1"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			fakeEnv(t, home, map[int][]string{300: {"PATH=/bin"}})
			if name == "--settings" {
				readEnv := processAccess
				processAccess = func(pid int, claude bool) ([]string, claudeSrc, bool) {
					env, _, ok := readEnv(pid, claude)
					return env, claudeSrc{Settings: path}, ok && claude
				}
			}
			old := usageDetector
			t.Cleanup(func() { usageDetector = old })
			usageDetector = agentdetect.New(agentdetect.WithProcessSource(usageProcs{
				{PID: 100, PPID: 1, PGRP: 100, TPGID: 300, TTY: "pts/1", Args: []string{"-bash"}},
				{PID: 300, PPID: 100, PGRP: 300, TPGID: 300, TTY: "pts/1", Args: args},
			}))
			host := usageHostFixture{[]term.Session{{Name: "work", Panes: []term.Pane{{Active: true, Ref: term.PaneRef{Command: "bash", PID: 100}}}}}}
			r, err := usageSnapshot(t.Context(), host)
			if err != nil {
				t.Fatal(err)
			}
			if s := r.Sessions[0]; s.AuthMode != "api" || !s.CustomEndpoint {
				t.Fatalf("Claude access = %q, custom endpoint = %t", s.AuthMode, s.CustomEndpoint)
			}
		})
	}
}
