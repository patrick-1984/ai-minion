package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Gate-1 C: the stop fallback of a stopped-looking profile never stops a
// unit that is registered for another profile.
func TestStopFallbackChecksUnitProfile(t *testing.T) {
	mine, other := t.TempDir(), t.TempDir()
	d := baseDown()
	d.call = func(context.Context, string) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	stopped := false
	d.output = func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "Environment") {
			return "A=b MINION_STATE_DIR=" + other + "\n", nil
		}
		return "active\n", nil
	}
	d.run = func(_ context.Context, _ string, args ...string) error {
		if len(args) > 1 && args[1] == "stop" {
			stopped = true
		}
		return nil
	}
	m, code := downJSON(t, d, "--state-dir", mine)
	if code != ExitStateChanged || stopped {
		t.Fatalf("other profile unit: %v %d stopped=%v", m, code, stopped)
	}
}

// A Worker that starts answering between the probes on a ConPTY host is not
// stopped without consent.
func TestStopFallbackLateWorkerNeedsConsent(t *testing.T) {
	d := baseDown()
	d.goos = "windows"
	n := 0
	sent := []string{}
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		sent = append(sent, op)
		n++
		if n == 1 {
			return nil, client.ErrWorkerNotRunning
		}
		return json.Marshal(agent.LiveStatusReply{StateDir: "/x", SessionHost: "conpty"})
	}
	m, code := downJSON(t, d)
	if code != ExitCountUnavailable {
		t.Fatalf("late worker: %v %d", m, code)
	}
	for _, op := range sent {
		if op == agent.OpLocalStop {
			t.Fatal("worker_stop was sent")
		}
	}
}

// A system unit this user may not stop is refused before the Worker takes a
// create hold (no lifecycle op).
func TestStopNeedsAdminBeforeHold(t *testing.T) {
	d := baseDown()
	d.call = func(context.Context, string) (json.RawMessage, error) {
		return json.Marshal(agent.LiveStatusReply{StateDir: "/x", Instance: "i", SessionHost: "tmux"})
	}
	called := false
	d.callArgs = func(context.Context, string, any) (json.RawMessage, error) { called = true; return nil, nil }
	d.output = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "--user" {
			return "inactive\n", nil
		}
		return "active\n", nil
	}
	m, code := downJSON(t, d)
	if code != ExitNeedsAdmin || called {
		t.Fatalf("%v %d lifecycle called=%v", m, code, called)
	}
}
