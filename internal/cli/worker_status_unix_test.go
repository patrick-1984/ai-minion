//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// runProductionWorker runs the real Worker (every default module, including
// the local control socket) in the background and returns its stop function.
// The socket is found through XDG_RUNTIME_DIR, like a real CLI finds it.
func runProductionWorker(t *testing.T, env map[string]string) (stop func()) {
	t.Helper()
	// A short directory keeps the socket path within the unix limit.
	rtDir, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rtDir) })
	t.Setenv("XDG_RUNTIME_DIR", rtDir)

	deps := workerRunDeps{stderr: &lockedBuf{}, lookupEnv: envFrom(env), loadConfig: loadWorkerConfig}
	root := workerRoot(deps)
	root.SetArgs([]string{"worker", "run"})
	root.SetOut(&lockedBuf{})
	root.SetErr(&lockedBuf{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker run = %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("worker run did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

// productionStatus runs "worker status --json" with the production
// dependencies (the real local socket client). With waitLive it retries until
// a running Worker answers.
func productionStatus(t *testing.T, env map[string]string, stateDir string, waitLive bool) agent.StatusReport {
	t.Helper()
	sd := defaultWorkerStatusDeps()
	sd.lookupEnv = envFrom(env)
	sd.sessions = func(string) agent.StatusSessions { return nil }
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", stateDir)
		if err != nil {
			t.Fatal(err)
		}
		var r agent.StatusReport
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if r.Connection.Live || !waitLive || time.Now().After(deadline) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWorkerStatusAsksTheRunningWorkerOverItsSocket runs the production
// command against a production Worker: the connection state, the reason and
// the pairing data come from the running Worker, and they are gone again once
// it stops (AD-22).
func TestWorkerStatusAsksTheRunningWorkerOverItsSocket(t *testing.T) {
	srv := tmuxtest.New(t)
	state := t.TempDir()
	env := map[string]string{
		"MINION_STATE_DIR":      state,
		"MINION_TMUX_SOCKET":    srv.Socket(),
		"MINION_SERVER":         "https://127.0.0.1:1",
		"MINION_NAME":           "e2e",
		"MINION_PREVIEW_ONLY":   "true",
		"MINION_SHARE_EXISTING": "all",
	}
	stop := runProductionWorker(t, env)

	r := productionStatus(t, env, state, true)
	if !r.Connection.Live {
		t.Fatalf("no running Worker answered: %+v", r)
	}
	// Not enrolled, so the connector is idle and says why: data that only the
	// running Worker has.
	if r.Connection.State != string(agent.ConnIdle) || !strings.Contains(r.Connection.Reason, "enrolled") {
		t.Fatalf("connection = %+v", r.Connection)
	}
	if r.Mode != agent.ModeHub || r.PairingState != agent.PairingUnknown || r.PendingPairingCode != "" {
		t.Fatalf("report = %+v", r)
	}

	// The text form says the same and does not turn the unknown into "none".
	sd := defaultWorkerStatusDeps()
	sd.lookupEnv = envFrom(env)
	sd.sessions = func(string) agent.StatusSessions { return nil }
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--state-dir", state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "not available here") || strings.Contains(out, "none pending") {
		t.Fatalf("status = %s", out)
	}

	stop()
	after := productionStatus(t, env, state, false)
	if after.Connection.Live || after.Connection.State != agent.ConnUnknown {
		t.Fatalf("after the Worker stopped: %+v", after.Connection)
	}
}

// A Solo Worker reports its live connection state too.
func TestWorkerStatusOfARunningSoloWorker(t *testing.T) {
	srv := tmuxtest.New(t)
	state := t.TempDir()
	env := map[string]string{"MINION_STATE_DIR": state, "MINION_TMUX_SOCKET": srv.Socket()}
	runProductionWorker(t, env)
	r := productionStatus(t, env, state, true)
	if !r.Connection.Live || r.Connection.State != string(agent.ConnIdle) || r.Mode != agent.ModeSolo {
		t.Fatalf("report = %+v", r)
	}
}
