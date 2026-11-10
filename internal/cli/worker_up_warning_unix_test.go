//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

type mapFlags map[string]string

func (m mapFlags) Lookup(k string) (string, bool, error) { v, ok := m[k]; return v, ok, nil }

// The warning classification must not read MINION_INVITE_KEY_FILE: a FIFO
// without a writer blocks a read forever (S98-R2-02).
func TestSecretWarningsClassifyTheInvitationKeyWithoutReadingItsFile(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "invite.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFO: %v", err)
	}
	reg := config.NewRegistry()
	for _, s := range config.Default.ForScope(config.ScopeWorker) {
		reg.Declare(s)
	}
	env := func(k string) (string, bool) {
		if k == "MINION_INVITE_KEY_FILE" {
			return fifo, true
		}
		return "", false
	}
	done := make(chan []string, 1)
	go func() {
		w, _ := secretWarnings(reg, &config.Resolver{Registry: reg, Role: config.ScopeWorker, LookupEnv: env})
		done <- w
	}()
	select {
	case w := <-done:
		if len(w) != 0 {
			t.Errorf("warnings for a file source: %v", w)
		}
	case <-time.After(3 * time.Second):
		// Release the blocked reader so the test process stays clean.
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("secretWarnings blocked reading the invitation key file")
	}
}

func TestSecretWarningsLiteralKeyWarnsOnceWithoutReadingAnyFile(t *testing.T) {
	reg := config.NewRegistry()
	for _, s := range config.Default.ForScope(config.ScopeWorker) {
		reg.Declare(s)
	}
	env := func(k string) (string, bool) {
		if k == "MINION_INVITE_KEY_FILE" {
			return "/nonexistent/never-read", true
		}
		return "", false
	}
	res := &config.Resolver{Registry: reg, Role: config.ScopeWorker, LookupEnv: env, Flags: mapFlags{config.KeyInviteKey: "mnk_LITERAL.abcdef"}}
	w, warned := secretWarnings(reg, res)
	if len(w) != 1 || !warned || strings.Contains(w[0], "LITERAL") {
		t.Errorf("warnings %q warned=%v", w, warned)
	}
}

// Service mode must warn about a literal key before the first-run gate
// refuses, exactly once, and without the enrollment hook repeating it
// (S98-R2-03).
func TestWorkerUpServiceWarnsAboutALiteralKeyBeforeTheFirstRunRefusal(t *testing.T) {
	f, _ := serviceGate(t, false, false)
	f.hook("enroll", func(s agent.Settings) error { return nil })
	_, err := f.runIn(readFailer{t}, "--invite-key", "mnk_SVCFIRST.abcdef")
	if err == nil || !strings.Contains(err.Error(), "not set up yet") {
		t.Fatalf("err = %v", err)
	}
	got := f.stderr.String()
	if n := strings.Count(got, "process list"); n != 1 {
		t.Errorf("%d warnings, want 1: %q", n, got)
	}
	if strings.Contains(got, "SVCFIRST") {
		t.Errorf("the key leaked: %q", got)
	}
	if calls := f.runCalls(); len(calls) != 0 {
		t.Errorf("service commands ran: %v", calls)
	}
}
