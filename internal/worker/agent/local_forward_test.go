package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// SC-5: the local socket forwards session-control requests whose target is on
// another Worker to the Hub, with the calling session as source_session.

func TestForwardsToHubOnlyForeignSessionControlTargets(t *testing.T) {
	self, other := ops.NewUUIDv7(), ops.NewUUIDv7()
	target := func(worker string) json.RawMessage {
		return mustJSON(t, map[string]any{"target": ops.Target{WorkerID: worker, SessionName: "x"}})
	}
	for _, tc := range []struct {
		name string
		op   string
		args json.RawMessage
		self string
		want bool
	}{
		{"workers_list", ops.OpWorkersList, nil, self, true},
		{"sessions_list of another Worker", ops.OpSessionsList, mustJSON(t, map[string]string{"worker_id": other}), self, true},
		{"sessions_list of this Worker", ops.OpSessionsList, mustJSON(t, map[string]string{"worker_id": self}), self, false},
		{"sessions_list plain", ops.OpSessionsList, nil, self, false},
		{"snapshot on another Worker", ops.OpSessionSnapshot, target(other), self, true},
		{"send_text on another Worker", ops.OpSendText, target(other), self, true},
		{"answer on another Worker without an own id", ops.OpAnswer, target(other), "", true},
		{"snapshot on this Worker", ops.OpSessionSnapshot, target(self), self, false},
		{"snapshot without a Worker", ops.OpSessionSnapshot, target(""), self, false},
		{"pipe_send stays with the pipe", ops.OpPipeSend, target(other), self, false},
		{"session_create is not forwarded", ops.OpSessionCreate, mustJSON(t, map[string]string{"worker_id": other}), self, false},
	} {
		if got := forwardsToHub(tc.op, tc.args, tc.self); got != tc.want {
			t.Errorf("%s: forwardsToHub = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSessionOfPaneMapsThePaneIDOfTheWorkersHost(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("one", "bash")
	ref := e.host.addSession("boss", "claude")
	if got := e.ex.sessionOfPane(context.Background(), ref.ID); got != "boss" {
		t.Fatalf("sessionOfPane(%s) = %q, want boss", ref.ID, got)
	}
	for _, p := range []string{"", "%999"} {
		if got := e.ex.sessionOfPane(context.Background(), p); got != "" {
			t.Errorf("sessionOfPane(%q) = %q, want none", p, got)
		}
	}
}

// fwdEnv is a local executor whose call client is linked to a fake Hub that
// answers every call with result.
func fwdEnv(t *testing.T, result any) (*env, localExecutor, *callHub) {
	t.Helper()
	e := newEnv(t)
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": result})
	})
	c := NewCallClient(CallConfig{})
	t.Cleanup(h.link(c))
	return e, localExecutor{ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return c }}, h
}

func TestForeignTargetIsForwardedWithTheSourceSession(t *testing.T) {
	e, l, h := fwdEnv(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	ref := e.host.addSession("boss", "claude")
	other := ops.NewUUIDv7()
	args := mustJSON(t, ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: other, SessionName: "build"}, Text: "hi"})
	rq := localapiRequest(ops.OpSendText, args)
	rq.Pane = ref.ID
	raw, err := l.Execute(context.Background(), rq, func(json.RawMessage) {})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.WriteResult
	if json.Unmarshal(raw, &res) != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %s", raw)
	}
	req := h.requestAt(0)
	if string(req["op"]) != `"send_text"` || string(req["source_session"]) != `"boss"` || !strings.Contains(string(req["args"]), other) {
		t.Fatalf("the Hub got %v", req)
	}
	if len(e.host.pastes()) != 0 {
		t.Fatal("a forwarded request was typed into a local pane")
	}
}

func TestForwardWithAnUnknownPaneSendsNoSourceSession(t *testing.T) {
	_, l, h := fwdEnv(t, ops.WorkersListResult{})
	rq := localapiRequest(ops.OpWorkersList, nil)
	rq.Pane = "%999"
	if _, err := l.Execute(context.Background(), rq, func(json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.requestAt(0)["source_session"]; ok {
		t.Fatalf("the Hub got a source_session for an unknown pane: %v", h.requestAt(0))
	}
}

func TestSameWorkerTargetStaysLocal(t *testing.T) {
	e, l, h := fwdEnv(t, struct{}{})
	self := ops.NewUUIDv7()
	e.policy.SetWorkerID(self)
	e.host.addSession("work", "bash")
	for _, w := range []string{"", self} {
		args := mustJSON(t, ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: w, SessionName: "work"}})
		if _, err := l.Execute(context.Background(), localapiRequest(ops.OpSessionSnapshot, args), func(json.RawMessage) {}); err != nil {
			t.Fatalf("worker %q: %v", w, err)
		}
	}
	h.mu.Lock()
	n := len(h.requests)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d requests reached the Hub", n)
	}
}

func TestForeignTargetWithoutAHubConnectionIsAClearError(t *testing.T) {
	e := newEnv(t)
	args := mustJSON(t, ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "x"}})
	for name, calls := range map[string]func() PipeCaller{
		"no call client":   nil,
		"not connected":    func() PipeCaller { return NewCallClient(CallConfig{}) },
		"nil call service": func() PipeCaller { return nil },
	} {
		l := localExecutor{ex: e.ex, log: discardLogger(), calls: calls}
		_, err := l.Execute(context.Background(), localapi.Request{Op: ops.OpSessionSnapshot, Actions: localapi.FullActions(), Args: args}, func(json.RawMessage) {})
		if proto.CodeOf(err) != proto.CodeWorkerOffline || !strings.Contains(err.Error(), "not connected to a Hub") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
