package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func downJSON(t *testing.T, d workerDownDeps, args ...string) (map[string]any, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerDownCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"worker", "down", "--json"}, args...), &out, &errOut)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("stdout %q: %v (stderr %q)", out.String(), err, errOut.String())
	}
	return m, code
}

// An older Worker that accepts worker_stop and ignores its arguments, on a
// ConPTY host: the CLI never sends worker_stop and exits 29.
func TestLifecycleLegacyWorkerGets29(t *testing.T) {
	d := baseDown()
	d.goos = "windows"
	var sent []string
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		sent = append(sent, op)
		if op == agent.OpLocalStatus {
			return json.Marshal(agent.LiveStatusReply{StateDir: "/s", Instance: "i1", SessionHost: "conpty"})
		}
		return json.RawMessage(`{"stopping":true}`), nil
	}
	d.callArgs = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		sent = append(sent, op)
		return nil, proto.NewError(proto.CodeUnsupportedOp, "unknown op "+op)
	}
	m, code := downJSON(t, d)
	if code != ExitCountUnavailable || m["code"] != "count_unavailable" {
		t.Fatalf("legacy ConPTY Worker = %v, %d", m, code)
	}
	if slices.Contains(sent, agent.OpLocalStop) {
		t.Fatalf("the bare worker_stop was sent: %v", sent)
	}
	// A tmux host loses nothing: the bare stop is fine there.
	d.goos = "linux"
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		sent = append(sent, op)
		if op == agent.OpLocalStatus {
			if slices.Contains(sent, agent.OpLocalStop) {
				return nil, client.ErrWorkerNotRunning
			}
			return json.Marshal(agent.LiveStatusReply{StateDir: "/s", Instance: "i1", SessionHost: "tmux"})
		}
		return json.RawMessage(`{"stopping":true}`), nil
	}
	if m, code := downJSON(t, d); code != ExitOK || m["stopped"] != true {
		t.Fatalf("legacy tmux Worker = %v, %d", m, code)
	}
}

// The Windows service runs but nothing answers on the pipe: 29, and with
// --accept-unknown-loss it is stopped through the service manager.
func TestLifecycleUnreachableRunningService29(t *testing.T) {
	d := baseDown()
	d.goos = "windows"
	d.call = func(context.Context, string) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	d.callArgs = func(context.Context, string, any) (json.RawMessage, error) {
		t.Fatal("no Worker answers")
		return nil, nil
	}
	d.windowsRunning = func() bool { return true }
	stopped := false
	d.windowsService = func() (bool, error) { stopped = true; return true, nil }
	if m, code := downJSON(t, d); code != ExitCountUnavailable || stopped {
		t.Fatalf("unreachable = %v, %d, stopped %t", m, code, stopped)
	}
	if m, code := downJSON(t, d, "--accept-unknown-loss"); code != ExitOK || !stopped {
		t.Fatalf("accepted = %v, %d", m, code)
	}
}

// Two ConPTY sessions would end: 23 with sessions_lost 2; --accept-loss 2
// stops.
func TestLifecycleSessionsWouldEnd23(t *testing.T) {
	d := baseDown()
	d.goos = "windows"
	var gone bool
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		if gone {
			return nil, client.ErrWorkerNotRunning
		}
		return json.Marshal(agent.LiveStatusReply{StateDir: "/s", Instance: "i1", SessionHost: "conpty"})
	}
	d.callArgs = func(_ context.Context, op string, args any) (json.RawMessage, error) {
		if op != "worker_lifecycle+lifecycle.v1" {
			t.Fatalf("op %q", op)
		}
		a := args.(agent.LocalLifecycleArgs)
		if a.AcceptLoss < 2 {
			det, _ := json.Marshal(map[string]any{"reason": agent.ReasonSessionsWouldEnd, "sessions_lost": 2})
			pe := proto.NewError(proto.CodeConflict, "2 sessions would end; nothing was done")
			pe.Details = det
			return nil, pe
		}
		gone = true
		return json.Marshal(agent.LifecycleResult{Action: "stop", SessionsLost: 2, Exiting: true})
	}
	m, code := downJSON(t, d)
	if code != ExitSessionsWouldEnd || m["sessions_lost"] != float64(2) {
		t.Fatalf("down = %v, %d", m, code)
	}
	if m, code := downJSON(t, d, "--accept-loss", "2"); code != ExitOK || m["stopped"] != true {
		t.Fatalf("accept 2 = %v, %d", m, code)
	}
}

// Restart: a new instance answers for the same state directory.
func TestRestartSameStateDirNewInstance(t *testing.T) {
	var mu sync.Mutex
	inst := "i1"
	d := workerRestartDeps{sleep: func(time.Duration) {}, wait: 2 * time.Second}
	d.call = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case agent.OpLocalStatus:
			return json.Marshal(agent.LiveStatusReply{StateDir: "/state", Instance: inst})
		case "worker_lifecycle+lifecycle.v1":
			inst = "i2"
			return json.Marshal(agent.LifecycleResult{Action: "restart", SessionsLost: 0, Exiting: true})
		}
		return nil, errors.New("unexpected " + op)
	}
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerRestartCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, []string{"worker", "restart", "--json"}, &out, &errOut)
	var m map[string]any
	_ = json.Unmarshal(out.Bytes(), &m)
	if code != ExitOK || m["instance"] != "i2" || m["old_instance"] != "i1" || m["state_dir"] != "/state" {
		t.Fatalf("restart = %v, %d (%s)", m, code, errOut.String())
	}
	// No Worker: 24.
	d.call = func(context.Context, string, any) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	root = &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerRestartCmd(d))
	out.Reset()
	if code := execute(context.Background(), root, []string{"worker", "restart", "--json"}, &out, &errOut); code != ExitNotRunning || !strings.Contains(out.String(), "not_running") {
		t.Fatalf("no Worker: %d %s", code, out.String())
	}
}
