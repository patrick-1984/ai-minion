//go:build unix

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func TestUsageReportKeepsSessionsWhenAccountsGrow(t *testing.T) {
	now := time.Now()
	at := now.UnixMilli()
	r := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: at, Complete: true,
		Sessions: []ops.UsageSession{{ID: ops.UsageID("s"), Name: "a", AuthMode: "unknown", Source: "session-host", ObservedAt: at}}}
	for i := 0; i < 200; i++ {
		r.Accounts = append(r.Accounts, ops.UsageAccount{ID: ops.UsageID("a", fmt.Sprint(i)), Provider: "Anthropic", Label: "a@example.org", AuthMode: "subscription", Source: "native-account", ObservedAt: at})
	}
	// One account that cannot be represented, one with a label that is too long.
	r.Accounts = append(r.Accounts, ops.UsageAccount{ID: "bad id", Provider: "x", AuthMode: "api", Source: "s", ObservedAt: at})
	r.Accounts = append(r.Accounts, ops.UsageAccount{ID: ops.UsageID("long"), Provider: "x", AuthMode: "api", Source: "session-host", Label: strings.Repeat("l", 400), ObservedAt: at})
	if err := fitUsageReport(&r, now); err != nil {
		t.Fatal(err)
	}
	if len(r.Sessions) != 1 || len(r.Accounts) != maxUsageAccounts {
		t.Fatalf("sessions %d accounts %d", len(r.Sessions), len(r.Accounts))
	}
	for _, a := range r.Accounts {
		if a.ID == "bad id" {
			t.Fatal("an invalid account stayed")
		}
		if a.ID == ops.UsageID("long") && a.Label != "" {
			t.Fatal("a bad label stayed")
		}
	}
	if !strings.Contains(fmt.Sprint(r.Accounts), ops.UsageID("long")) {
		t.Fatal("the session-derived account was cut before the discovered ones")
	}
}

func TestWrapperShellIsNeverTheProcessWhoseEnvironmentIsRead(t *testing.T) {
	fakeEnv(t, t.TempDir(), map[int][]string{
		200: {"PATH=/bin"},                       // the wrapper shell: no API setting
		300: {"ANTHROPIC_API_KEY=sk-ant-SECRET"}, // the agent
	})
	old := usageDetector
	t.Cleanup(func() { usageDetector = old })
	tree := usageProcs{
		{PID: 100, PPID: 1, PGRP: 100, TPGID: 300, TTY: "pts/1", Args: []string{"-bash"}},
		{PID: 200, PPID: 100, PGRP: 300, TPGID: 300, TTY: "pts/1", Args: []string{"bash", "-c", "claude --resume x"}},
		{PID: 300, PPID: 200, PGRP: 300, TPGID: 300, TTY: "pts/1", Args: []string{"claude", "--resume", "x"}},
	}
	usageDetector = agentdetect.New(agentdetect.WithProcessSource(tree))
	host := usageHostFixture{[]term.Session{{Name: "w", Panes: []term.Pane{{Active: true, Ref: term.PaneRef{Command: "bash", PID: 100}}}}}}
	r, err := usageSnapshot(context.Background(), host, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := r.Sessions[0]; s.AuthMode != "api" {
		t.Fatalf("API settings of the child lost: %+v", s)
	}
	// Without the real agent process the access is unknown, not the shell's.
	usageDetector = agentdetect.New(agentdetect.WithProcessSource(tree[:2]))
	r, err = usageSnapshot(context.Background(), host, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := r.Sessions[0]; s.Harness != "claude" || s.AuthMode != "unknown" {
		t.Fatalf("session %+v", s)
	}
}

func TestProcessEnvironmentIsReadThroughTheVerifiedDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs")
	}
	self := os.Getpid()
	if _, ok := readProcessEnv(self); !ok {
		t.Fatal("own environment unreadable")
	}
	old := workerUID
	t.Cleanup(func() { workerUID = old })
	workerUID = func() int { return os.Getuid() + 1 }
	if _, ok := readProcessEnv(self); ok {
		t.Fatal("another user's process read")
	}
	workerUID = old
	oldRoot := procRoot
	t.Cleanup(func() { procRoot = oldRoot })
	procRoot = t.TempDir() // a vanished process: no directory
	if _, ok := readProcessEnv(self); ok {
		t.Fatal("read without a process directory")
	}
}

func TestManagedSettingsFailClosedAndNeverStall(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "managed-settings.json")
	if ClaudeManagedOverrides(file) {
		t.Fatal("nothing managed")
	}
	if err := os.WriteFile(file, []byte(`{"permissions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ClaudeManagedOverrides(file) {
		t.Fatal("a managed file without a status line or hooks policy")
	}
	drop := filepath.Join(dir, "managed-settings.d")
	if err := os.Mkdir(drop, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drop, "10-hooks.json"), []byte(`{"disableAllHooks":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ClaudeManagedOverrides(file) {
		t.Fatal("a drop-in policy was missed")
	}
	_ = os.Remove(filepath.Join(drop, "10-hooks.json"))
	if os.Getuid() != 0 {
		_ = os.Chmod(drop, 0)
		t.Cleanup(func() { _ = os.Chmod(drop, 0o755) })
		if !ClaudeManagedOverrides(file) {
			t.Fatal("an unreadable policy directory must count as overridden")
		}
		_ = os.Chmod(drop, 0o755)
	}
	// A named pipe in place of the file: overridden, and no stall.
	_ = os.Remove(file)
	if err := syscall.Mkfifo(file, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan bool, 1)
	go func() { done <- ClaudeManagedOverrides(file) }()
	select {
	case got := <-done:
		if !got {
			t.Fatal("a non-regular managed file must count as overridden")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a named pipe stalled the policy check")
	}
}

func TestExporterStateIgnoresAProfileFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "settings.json"), 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan string, 1)
	go func() { done <- claudeExporterStateIn(dir) }()
	select {
	case got := <-done:
		if got != ops.UsageExporterAbsent {
			t.Fatalf("state %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a named pipe stalled the inventory")
	}
}
