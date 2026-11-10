//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// The local clients (CLI and MCP, through client.Local) against a Worker daemon of
// the previous generation and one of the current. The "old" daemon is the real
// Executor behind the entry point as it was before capability-qualified requests:
// it hands the op name and the arguments to Executor.Execute and, like every Worker
// of its time, ignores the argument fields it does not know. Nothing about its
// answer is mocked; what is asserted is what reached the terminal.

// oldLocalExecutor is the localapi entry point of a Worker that predates
// capability-qualified operations.
type oldLocalExecutor struct{ ex *Executor }

func (o oldLocalExecutor) Execute(ctx context.Context, rq localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
	if IsPipeOp(rq.Op) { // served by the pipe service, which this fixture does not run
		return nil, apiErr(proto.CodeUnsupportedOp, "", "the pipe is not running on this Worker")
	}
	return o.ex.Execute(ctx, Request{Op: rq.Op, Actions: rq.Actions, Args: rq.Args})
}

// serveLocal serves ex on a private unix socket with the given entry point and
// returns a client for it.
func serveLocal(t *testing.T, exec localapi.Executor) *client.LocalBackend {
	t.Helper()
	dir, err := os.MkdirTemp("", "lc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "worker.sock")
	l, err := localapi.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := localapi.New(localapi.Config{Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(context.Background()); <-done })
	return client.Local(client.WithSocketPath(path))
}

func reviewedFromSnapshot(t *testing.T, e *env) ops.ReviewedDialog {
	t.Helper()
	raw := e.reviewedOf("work")
	b, _ := json.Marshal(raw)
	var rd ops.ReviewedDialog
	if err := json.Unmarshal(b, &rd); err != nil {
		t.Fatal(err)
	}
	return rd
}

func callLocal(t *testing.T, c *client.LocalBackend, op string, args any) (json.RawMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return c.Call(ctx, op, args)
}

// verbatimFake is a host that guarantees the verbatim paste.
type verbatimFake struct{ *fakeHost }

func (verbatimFake) VerbatimPaste() bool { return true }

func TestLocalClientsRefuseAnOlderWorkerBeforeAnyEffect(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "Bash command", "  echo ok", "Do you want to proceed?", "❯ 1. Yes", "  2. No", "Esc to cancel")
	e.cls.set("claude", "agent:claude", ops.PaneStateDialog, dialogChoices...)
	old := serveLocal(t, oldLocalExecutor{e.ex})
	reviewed := reviewedFromSnapshot(t, e)
	wa := func() ops.WriteArgs { return ops.WriteArgs{RequestID: ops.NewUUIDv7()} }
	target := ops.Target{SessionName: "work"}

	for name, call := range map[string]func() (json.RawMessage, error){
		"answer": func() (json.RawMessage, error) {
			return callLocal(t, old, ops.OpAnswer, ops.AnswerArgs{WriteArgs: wa(), Target: target, Choice: "1", Expect: reviewed})
		},
		"answer with another pane's binding": func() (json.RawMessage, error) {
			other := reviewed
			other.PaneID = "%999"
			return callLocal(t, old, ops.OpAnswer, ops.AnswerArgs{WriteArgs: wa(), Target: target, Choice: "1", Expect: other})
		},
		"send_text submit": func() (json.RawMessage, error) {
			return callLocal(t, old, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target, Text: "hello", Submit: true})
		},
		"multi-line text": func() (json.RawMessage, error) {
			return callLocal(t, old, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target, Text: "one\ntwo"})
		},
		"pipe_send with a tab": func() (json.RawMessage, error) {
			return callLocal(t, old, ops.OpPipeSend, ops.PipeSendArgs{WriteArgs: wa(), Target: target, Text: "a\tb"})
		},
	} {
		raw, err := call()
		if !errors.Is(err, client.ErrWorkerTooOld) || !strings.Contains(err.Error(), "Nothing was sent") {
			t.Errorf("%s to an older Worker: %s, %v, want ErrWorkerTooOld", name, raw, err)
		}
	}
	if len(e.host.keys()) != 0 || len(e.host.pastes()) != 0 {
		t.Fatalf("an older Worker received guarded requests: keys %v pastes %v", e.host.keys(), e.host.pastes())
	}

	// The dialog is gone; what an older Worker handles the same way still works: a plain one-line
	// text, a key, a read. The client does not refuse what it need not.
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	if raw, err := callLocal(t, old, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target, Text: "plain line"}); err != nil {
		t.Fatalf("a plain text: %s, %v", raw, err)
	}
	if _, err := callLocal(t, old, ops.OpSendKeys, ops.SendKeysArgs{WriteArgs: wa(), Target: target, Keys: []string{"Enter"}}); err != nil {
		t.Fatalf("send_keys: %v", err)
	}
	if _, err := callLocal(t, old, ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: target}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := len(e.host.pastes()); got != 1 {
		t.Fatalf("pastes = %v, want only the plain text", e.host.pastes())
	}
}

// The same requests reach a current Worker and are delivered, bound to the dialog
// and atomic; and a current Worker whose host does not guarantee the verbatim
// paste refuses a multi-line text itself, so the requirement is checked against
// what the daemon really implements, not against a claim of the client.
func TestLocalClientsReachACurrentWorkerAndItChecksItsOwnFeatures(t *testing.T) {
	e := newEnv(t)
	e.ex.host = verbatimFake{e.host}
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "Bash command", "  echo ok", "Do you want to proceed?", "❯ 1. Yes", "  2. No", "Esc to cancel")
	e.cls.set("claude", "agent:claude", ops.PaneStateDialog, dialogChoices...)
	srv := localExecutor{ex: e.ex, log: discardLogger()}
	cur := serveLocal(t, srv)
	reviewed := reviewedFromSnapshot(t, e)
	target := ops.Target{SessionName: "work"}
	wa := func() ops.WriteArgs { return ops.WriteArgs{RequestID: ops.NewUUIDv7()} }

	raw, err := callLocal(t, cur, ops.OpAnswer, ops.AnswerArgs{WriteArgs: wa(), Target: target, Choice: "1", Expect: reviewed})
	var res ops.WriteResult
	if err != nil || json.Unmarshal(raw, &res) != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("answer on a current Worker: %s, %v", raw, err)
	}
	// A stale binding is still refused by the native guard, not by the client.
	stale := reviewed
	stale.PaneID = "%999"
	raw, err = callLocal(t, cur, ops.OpAnswer, ops.AnswerArgs{WriteArgs: wa(), Target: target, Choice: "1", Expect: stale})
	if err != nil || json.Unmarshal(raw, &res) != nil || res.Outcome != ops.OutcomeFailedNotSent {
		t.Fatalf("a stale binding on a current Worker: %s, %v", raw, err)
	}
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	raw, err = callLocal(t, cur, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target, Text: "one\ntwo", Submit: true})
	if err != nil || json.Unmarshal(raw, &res) != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("a multi-line submit on a current Worker: %s, %v", raw, err)
	}
	if ev := e.host.eventLog(); len(ev) < 2 || ev[len(ev)-2] != "paste "+ref.ID || !strings.HasPrefix(ev[len(ev)-1], "keys "+ref.ID) {
		t.Fatalf("the submit was not one paste then Enter on one pane: %v", ev)
	}

	// A host without the verbatim paste: the Worker says so itself.
	e2 := newEnv(t)
	ref2 := e2.host.addSession("work", "claude")
	_ = ref2
	e2.cls.set("claude", "agent:claude", ops.PaneStateStable)
	cur2 := serveLocal(t, localExecutor{ex: e2.ex, log: discardLogger()})
	_, err = callLocal(t, cur2, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target, Text: "one\ntwo"})
	if err == nil || !strings.Contains(err.Error(), "does not announce the feature") {
		t.Fatalf("a multi-line text on a Worker whose host is not verbatim: %v", err)
	}
	if len(e2.host.pastes()) != 0 {
		t.Fatalf("a Worker without the feature pasted: %v", e2.host.pastes())
	}
}

var _ term.SessionHost = verbatimFake{}
