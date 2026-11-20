package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// allow_new_sessions off refuses session_create at the one entry point, for
// the Hub (op server), the local socket (CLI, mn, MCP) and the executor
// itself; no session is created, existing sessions and other writes work, and
// the Hub cannot lift it.
func TestNewSessionsBlockHoldsForEveryOrigin(t *testing.T) {
	o := newOriginEnv(t, "none")
	allowed := true
	o.policy.SetNewSessionsCheck(func() bool { return allowed })
	l := localExecutor{ex: o.ex, log: discardLogger()}
	before := len(o.host.sessions)
	create := func() ops.SessionCreateArgs {
		return ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: "fresh"}
	}

	user := &proto.Origin{Kind: proto.OriginUser, UserID: ownerA}
	allowed = false
	// Hub origin.
	if typ, code := o.serve(ops.OpSessionCreate, create(), user); typ != proto.TypeError || code != proto.CodeForbidden {
		t.Fatalf("Hub: %s %s", typ, code)
	}
	// Local origin (the socket of the CLI, mn and MCP).
	_, err := l.Execute(context.Background(), localapiRequest(ops.OpSessionCreate, mustJSON(t, create())), func(json.RawMessage) {})
	if proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("local: %v", err)
	}
	// The executor, with every action.
	_, err = o.ex.Execute(context.Background(), Request{Op: ops.OpSessionCreate, Actions: []ops.Action{ops.ActionRead, ops.ActionOperate}, Args: mustJSON(t, create())})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeForbidden || ae.Reason != proto.CodeNewSessionsBlocked {
		t.Fatalf("executor: %v", err)
	}
	if got := len(o.host.sessions); got != before {
		t.Fatalf("a session was created while blocked (%d -> %d)", before, got)
	}
	// Other writes are not affected.
	if typ, _ := o.serve(ops.OpSendText, sendText("work", "hi", false), user); typ == proto.TypeError {
		t.Fatal("the block refused a write that creates nothing")
	}

	allowed = true
	if typ, code := o.serve(ops.OpSessionCreate, create(), user); typ == proto.TypeError {
		t.Fatalf("allowed: %s %s", typ, code)
	}
	if got := len(o.host.sessions); got != before+1 {
		t.Fatalf("sessions %d, want %d", got, before+1)
	}
}

// The status shows what a running Worker enforces, "unknown" when a running
// Worker cannot confirm it (an older Worker, a failing socket), and the saved
// value when no Worker runs.
func TestStatusNewSessions(t *testing.T) {
	ctx := context.Background()
	get := func(in StatusInputs) string { return BuildStatus(ctx, in).NewSessions }
	if got := get(StatusInputs{NewSessions: "off", Live: fakeLive{err: ErrNoLiveWorker}}); got != "off" {
		t.Errorf("no Worker: %q, want the saved off", got)
	}
	if got := get(StatusInputs{NewSessions: "off", Live: fakeLive{ls: LiveStatus{NewSessions: "on"}}}); got != "on" {
		t.Errorf("running Worker: %q, want what it enforces", got)
	}
	if got := get(StatusInputs{NewSessions: "off", Live: fakeLive{ls: LiveStatus{}}}); got != NewSessionsUnknown {
		t.Errorf("an older running Worker: %q, want unknown", got)
	}
	if got := get(StatusInputs{NewSessions: "off", Live: fakeLive{err: errors.New("socket broken")}}); got != NewSessionsUnknown {
		t.Errorf("a failing socket: %q, want unknown", got)
	}
}
