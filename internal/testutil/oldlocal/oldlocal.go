//go:build unix

// Package oldlocal is a test double of a local Worker daemon of the previous
// generation: it serves the control socket, runs the ops it knows by their plain
// names and, like every Worker of its time, ignores the argument fields it does
// not know (AD-4). It does not know capability-qualified operation names
// ("answer+answer.reviewed"): it refuses them as unsupported operations, which
// is what the real previous-generation daemon does (its Executor looks the op up
// by name). It exists so that the CLI and MCP tests can show what a current
// client sends to an older daemon and what reaches its terminal; the real
// Executor behind the same entry point is exercised in internal/worker/agent.
//
// Owner: Story 2.9 (security integration, spine AM-13).
package oldlocal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// Daemon is a running old-style daemon.
type Daemon struct {
	// Backend is a client on its socket.
	Backend *client.LocalBackend

	mu   sync.Mutex
	runs []Run
}

// Run is one op the daemon actually ran, with the arguments as received.
type Run struct {
	Op   string
	Args json.RawMessage
}

// Runs returns what the daemon has run: the effects on its terminal.
func (d *Daemon) Runs() []Run {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Run(nil), d.runs...)
}

// New starts the daemon on a private socket for the life of the test.
func New(t *testing.T) *Daemon {
	t.Helper()
	d := &Daemon{}
	dir, err := os.MkdirTemp("", "old")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "worker.sock")
	l, err := localapi.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := localapi.New(localapi.Config{Executor: localapi.ExecutorFunc(d.execute)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(context.Background()); <-done })
	d.Backend = client.Local(client.WithSocketPath(path))
	return d
}

func (d *Daemon) execute(_ context.Context, rq localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
	known := map[string]bool{ops.OpAnswer: true, ops.OpSendText: true, ops.OpSendKeys: true, ops.OpSessionSnapshot: true, ops.OpSessionsList: true}
	if !known[rq.Op] {
		return nil, proto.NewError(proto.CodeUnsupportedOp, "the Worker does not implement this operation")
	}
	d.mu.Lock()
	d.runs = append(d.runs, Run{Op: rq.Op, Args: append(json.RawMessage(nil), rq.Args...)})
	d.mu.Unlock()
	// Whatever the arguments say, the old Worker reports the write delivered.
	return json.Marshal(ops.WriteResult{Outcome: ops.OutcomeDelivered})
}
