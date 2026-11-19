package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// C7: "worker stop --state-dir D" stops only the Worker of D. The Worker of
// another state directory is left running (no lifecycle op, no stop) and the
// command exits 27; the Worker of D is stopped.
func TestWorkerStopStateDirBindsProfile(t *testing.T) {
	mine, other := t.TempDir(), t.TempDir()
	d := baseDown()
	running := other
	var sent []string
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		if running == "" {
			return nil, client.ErrWorkerNotRunning
		}
		sent = append(sent, op)
		return json.Marshal(agent.LiveStatusReply{StateDir: running, Instance: "i1", SessionHost: "tmux"})
	}
	d.callArgs = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		sent = append(sent, op)
		running = ""
		return json.Marshal(agent.LifecycleResult{Action: "stop", Exiting: true})
	}
	m, code := downJSON(t, d, "--state-dir", mine)
	if code != ExitStateChanged || m["code"] != "state_changed" || m["live_state_dir"] != other {
		t.Fatalf("other profile = %v, %d", m, code)
	}
	if len(sent) != 1 || sent[0] != agent.OpLocalStatus {
		t.Fatalf("more than the status was sent: %v", sent)
	}
	running = mine
	if m, code := downJSON(t, d, "--state-dir", mine); code != ExitOK || m["stopped"] != true {
		t.Fatalf("own profile = %v, %d", m, code)
	}
	if _, code := downJSON(t, d, "--state-dir="); code != ExitUsage {
		t.Fatalf("empty --state-dir = %d", code)
	}
}

// C7: "worker restart --state-dir D" restarts only the Worker of D.
func TestWorkerRestartStateDirBindsProfile(t *testing.T) {
	mine, other := t.TempDir(), t.TempDir()
	lifecycle := 0
	d := workerRestartDeps{sleep: func(time.Duration) {}, wait: time.Second}
	d.call = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		if op == agent.OpLocalStatus {
			return json.Marshal(agent.LiveStatusReply{StateDir: other, Instance: "i1"})
		}
		lifecycle++
		return nil, errors.New("unexpected " + op)
	}
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerRestartCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, []string{"worker", "restart", "--json", "--state-dir", filepath.Join(mine, ".")}, &out, &errOut)
	var m map[string]any
	_ = json.Unmarshal(out.Bytes(), &m)
	if code != ExitStateChanged || m["code"] != "state_changed" || lifecycle != 0 {
		t.Fatalf("other profile = %v, %d, lifecycle ops %d", m, code, lifecycle)
	}
}
