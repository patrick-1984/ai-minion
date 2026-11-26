package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story W5 (audit B-13 and F13): a Hub request that names another Worker is
// refused, and a request id answers only the caller and the payload it was
// first used for.

var hubActions = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage}

func userOrigin(id string) *proto.Origin { return &proto.Origin{Kind: proto.OriginUser, UserID: id} }

// callFrom runs an op as the op server does for a Hub request with that origin;
// a nil origin is a local caller.
func (e *env) callFrom(origin *proto.Origin, op string, args any) (json.RawMessage, error) {
	e.t.Helper()
	return e.callRaw(origin, op, mustJSON(e.t, args))
}

func (e *env) callRaw(origin *proto.Origin, op string, raw json.RawMessage) (json.RawMessage, error) {
	e.t.Helper()
	return e.ex.Execute(context.Background(), Request{Op: op, Actions: hubActions, Args: raw, Origin: origin})
}

func newTargetEnv(t *testing.T) (e *env, self, other string) {
	t.Helper()
	e = newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "prompt")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	self, other = ops.NewUUIDv7(), ops.NewUUIDv7()
	e.policy.SetWorkerID(self)
	return e, self, other
}

// sessionRequests is one request of every session op for the Worker id.
func sessionRequests(id string) map[string]any {
	wa := func() ops.WriteArgs { return ops.WriteArgs{RequestID: ops.NewUUIDv7()} }
	tg := ops.Target{WorkerID: id, SessionName: "work"}
	return map[string]any{
		ops.OpSessionsList:    map[string]any{"worker_id": id},
		ops.OpSessionSnapshot: ops.SessionSnapshotArgs{Target: tg},
		ops.OpSessionCapture:  ops.SessionCaptureArgs{Target: tg},
		ops.OpSessionCreate:   map[string]any{"worker_id": id, "request_id": ops.NewUUIDv7(), "name": "fresh"},
		ops.OpSendText:        ops.SendTextArgs{WriteArgs: wa(), Target: tg, Text: "hello"},
		ops.OpSendKeys:        ops.SendKeysArgs{WriteArgs: wa(), Target: tg, Keys: []string{"Enter"}},
		ops.OpSetTask:         ops.SetTaskArgs{WriteArgs: wa(), Target: tg, Task: "a task"},
		ops.OpPipeSend:        ops.PipeSendArgs{WriteArgs: wa(), Target: tg, Text: "hello", MessageID: 7},
		ops.OpAnswer: ops.AnswerArgs{WriteArgs: wa(), Target: tg, Choice: "1", Expect: ops.ReviewedDialog{
			PaneID: "%1", PID: 1, Command: "claude", SessionCreated: 1,
			DialogDigest: "0000000000000000000000000000000000000000000000000000000000000000",
		}},
	}
}

func TestHubSessionOpNamingAnotherWorkerIsRefused(t *testing.T) {
	e, _, other := newTargetEnv(t)
	writes := 0
	for op, args := range sessionRequests(other) {
		_, err := e.callFrom(userOrigin("user-a"), op, args)
		wantAPIError(t, err, "forbidden", "")
		if o, ok := ops.Lookup(op); ok && o.Write {
			writes++
		}
	}
	if n := len(e.host.pastes()) + len(e.host.keys()); n != 0 {
		t.Fatalf("a request for another Worker wrote %d times", n)
	}
	e.host.mu.Lock()
	touched := e.host.snapshots + e.host.resolveCall
	e.host.mu.Unlock()
	if touched != 0 {
		t.Fatalf("a request for another Worker reached the terminal host %d times", touched)
	}
	if got, _ := e.host.ListSessions(context.Background()); len(got) != 1 {
		t.Fatalf("session_create for another Worker created a session: %+v", got)
	}
	// Every refused write leaves its audit row, with the Worker it named.
	rows := e.audit()
	if len(rows) != writes {
		t.Fatalf("audit rows = %d, want %d", len(rows), writes)
	}
	for _, r := range rows {
		if r.Outcome != "error" || r.Reason != "forbidden" {
			t.Fatalf("audit row = %+v", r)
		}
		if r.Op != ops.OpSessionCreate && r.Target != other+":work" {
			t.Fatalf("audit target = %q", r.Target)
		}
	}
}

func TestHubSessionOpNamingThisWorkerOrNoneIsServed(t *testing.T) {
	for _, tc := range []struct{ name, id string }{{"own id", "self"}, {"no id", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			e, self, _ := newTargetEnv(t)
			id := tc.id
			if id == "self" {
				id = self
			}
			for _, op := range []string{ops.OpSessionsList, ops.OpSessionSnapshot, ops.OpSessionCapture, ops.OpSendText, ops.OpSetTask} {
				args := sessionRequests(id)[op]
				if id == "" && op == ops.OpSessionsList {
					args = ops.SessionsListArgs{}
				}
				if _, err := e.callFrom(userOrigin("user-a"), op, args); err != nil {
					t.Fatalf("%s: %v", op, err)
				}
			}
			if n := len(e.host.pastes()); n != 1 {
				t.Fatalf("pastes = %d, want 1", n)
			}
		})
	}
}

// The local socket and a Worker without a Hub id are served as before: the
// local entry point forwards what names another Worker before it gets here.
func TestTargetWorkerCheckLeavesLocalCallersAndUnenrolledWorkersAlone(t *testing.T) {
	e, _, other := newTargetEnv(t)
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSendText} {
		if _, err := e.callFrom(nil, op, sessionRequests(other)[op]); err != nil {
			t.Fatalf("local %s: %v", op, err)
		}
	}
	e.policy.SetWorkerID("")
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSendText} {
		if _, err := e.callFrom(userOrigin("user-a"), op, sessionRequests(other)[op]); err != nil {
			t.Fatalf("no Hub id, %s: %v", op, err)
		}
	}
	if n := len(e.host.pastes()); n != 2 {
		t.Fatalf("pastes = %d, want 2", n)
	}
}

func TestRequestIDReusedByAnotherCallerConflicts(t *testing.T) {
	e, self, _ := newTargetEnv(t)
	args := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: self, SessionName: "work"}, Text: "hello"}
	first, err := e.callFrom(userOrigin("user-a"), ops.OpSendText, args)
	if err != nil {
		t.Fatal(err)
	}
	// The same caller again: the cached answer.
	again, err := e.callFrom(userOrigin("user-a"), ops.OpSendText, args)
	if err != nil || string(again) != string(first) {
		t.Fatalf("same caller: %s, %v; first %s", again, err, first)
	}
	others := []*proto.Origin{
		userOrigin("user-b"),
		{Kind: proto.OriginUser, UserID: "user-a", ActorID: "admin"},
		{Kind: proto.OriginAPIKey, UserID: "user-a", KeyID: "key-1"},
		workerOrigin(ops.NewUUIDv7(), "fp", "user-a"),
		nil, // a local caller
	}
	for i, o := range others {
		_, err := e.callFrom(o, ops.OpSendText, args)
		if err == nil {
			t.Fatalf("caller %d got the first caller's answer", i)
		}
		wantAPIError(t, err, "conflict", "")
	}
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1", n)
	}
	rec, ok, err := e.st.GetResult(context.Background(), args.RequestID)
	if err != nil || !ok || rec.Caller != "user user=user-a actor= key= worker=" || len(rec.PayloadHash) != 64 {
		t.Fatalf("stored record = %+v, %v, %v", rec, ok, err)
	}
}

func TestRequestIDReusedWithAnotherPayloadConflicts(t *testing.T) {
	e, _, _ := newTargetEnv(t)
	id := ops.NewUUIDv7()
	raw := func(text string) json.RawMessage {
		return json.RawMessage(`{"request_id":"` + id + `","target":{"session_name":"work"},"text":"` + text + `","submit":true}`)
	}
	first, err := e.callRaw(userOrigin("user-a"), ops.OpSendText, raw("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// The same request in another encoding (member order, space) is the same payload.
	same := json.RawMessage(`{ "submit": true, "text": "hello", "target": {"session_name": "work"}, "request_id": "` + id + `" }`)
	again, err := e.callRaw(userOrigin("user-a"), ops.OpSendText, same)
	if err != nil || string(again) != string(first) {
		t.Fatalf("same payload: %s, %v; first %s", again, err, first)
	}
	// Other text under the same id is never answered with the first outcome, and never sent.
	_, err = e.callRaw(userOrigin("user-a"), ops.OpSendText, raw("other text"))
	wantAPIError(t, err, "conflict", "")
	if got := e.host.pastes(); len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("pastes = %+v", got)
	}
	for _, r := range e.audit() {
		if r.Reason == "conflict" && r.Outcome == "error" {
			return
		}
	}
	t.Fatalf("the conflict left no audit row: %+v", e.audit())
}

// A row written before the caller and the payload were stored has neither, and
// answers by op, session and force as it always did.
func TestCachedRowWithoutCallerAndPayloadComparesAsBefore(t *testing.T) {
	e, _, _ := newTargetEnv(t)
	args := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Text: "hello"}
	old := store.ResultRecord{RequestID: args.RequestID, Op: ops.OpSendText, Session: "work", Result: []byte(`{"outcome":"delivered"}`)}
	if err := e.st.PutResult(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	raw, err := e.callFrom(userOrigin("user-b"), ops.OpSendText, args)
	if err != nil {
		t.Fatalf("an old row must answer: %v", err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("answer = %s, %v", raw, err)
	}
	if n := len(e.host.pastes()); n != 0 {
		t.Fatalf("an old row was sent again: %d pastes", n)
	}
	// Op, session and force still decide for it.
	forced := args
	forced.Force = true
	_, err = e.callFrom(userOrigin("user-b"), ops.OpSendText, forced)
	wantAPIError(t, err, "conflict", "")
}

// routedFrame is an op.request in the shape the Hub's Router sends it: the
// arguments as the typed operation encodes them (sessions_list and
// session_create name no Worker there) and, outside them, the Worker the Hub
// authorized and routed the request to (target_worker_id, review r1). An
// empty routed leaves the member out, as an older Hub does.
func routedFrame(t *testing.T, op, routed string) map[string]any {
	t.Helper()
	id := ops.NewUUIDv7()
	var args any = ops.SessionsListArgs{}
	if op == ops.OpSessionCreate {
		args = ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Name: "fresh"}
	}
	f := map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": id, "args": args,
		"actions": hubActions, "origin": userOrigin("user-a")}
	if routed != "" {
		f["target_worker_id"] = routed
	}
	return f
}

func TestRoutedDestinationOfTheFrameIsChecked(t *testing.T) {
	for _, tc := range []struct {
		name, routed string
		served       bool
	}{
		{"routed to another Worker", otherWorker, false},
		{"routed to this Worker", selfWorker, true},
		{"an older Hub names no destination", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOriginEnv(t, "none")
			for _, op := range []string{ops.OpSessionsList, ops.OpSessionCreate} {
				st := newFakeOpStream(t, routedFrame(t, op, tc.routed))
				o.srv.ServeStream(context.Background(), st)
				types := st.types(t)
				last := types[len(types)-1]
				if tc.served && last != proto.TypeOpResult {
					t.Fatalf("%s: %v, %+v; want it served", op, types, st.errorFrame(t))
				}
				if !tc.served && (last != proto.TypeError || st.errorFrame(t).Code != proto.CodeForbidden) {
					t.Fatalf("%s: %v; want forbidden", op, types)
				}
			}
			sessions, _ := o.host.ListSessions(context.Background())
			if want := map[bool]int{true: 2, false: 1}[tc.served]; len(sessions) != want {
				t.Fatalf("sessions = %d, want %d", len(sessions), want)
			}
		})
	}
}

// Arguments that name this Worker do not lift a frame that routes the request
// elsewhere, and a frame that names this Worker does not lift arguments that
// name another one.
func TestRoutedDestinationAndArgumentsAreBothChecked(t *testing.T) {
	o := newOriginEnv(t, "none")
	for _, tc := range []struct{ named, routed string }{{selfWorker, otherWorker}, {otherWorker, selfWorker}} {
		f := routedFrame(t, ops.OpSessionsList, tc.routed)
		f["args"] = map[string]any{"worker_id": tc.named}
		st := newFakeOpStream(t, f)
		o.srv.ServeStream(context.Background(), st)
		if fr := st.errorFrame(t); fr.Code != proto.CodeForbidden {
			t.Fatalf("named %s, routed %s: %+v; want forbidden", tc.named, tc.routed, fr)
		}
	}
	// A local caller never carries a destination of the Hub.
	if _, err := o.ex.Execute(context.Background(), Request{Op: ops.OpSessionsList, Actions: hubActions, Args: []byte(`{}`),
		TargetWorkerID: otherWorker}); err != nil {
		t.Fatalf("a local caller is checked as a Hub request: %v", err)
	}
}

// Two controller sessions of one source Worker that reuse a request id are two
// callers when the Hub names the admitted session (review r2): the second gets
// a conflict, never the first session's outcome, and nothing is sent twice. A
// request of an older Hub, which names no session, is compared as before.
func TestRequestIDReusedByAnotherSessionOfTheSameWorkerConflicts(t *testing.T) {
	e, self, _ := newTargetEnv(t)
	source := ops.NewUUIDv7()
	from := func(session string) *proto.Origin {
		o := workerOrigin(source, "fp", "user-a")
		o.SourceSession, o.SourceProof = session, proto.OriginProofClaimed
		return o
	}
	args := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: self, SessionName: "work"}, Text: "hello"}
	first, err := e.callFrom(from("planner"), ops.OpSendText, args)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.callFrom(from("planner"), ops.OpSendText, args)
	if err != nil || string(again) != string(first) {
		t.Fatalf("same session: %s, %v; first %s", again, err, first)
	}
	_, err = e.callFrom(from("reviewer"), ops.OpSendText, args)
	wantAPIError(t, err, "conflict", "")
	// The same Worker through a Hub that names no session: legacy matching.
	if legacy, err := e.callFrom(workerOrigin(source, "fp", "user-a"), ops.OpSendText, args); err != nil || string(legacy) != string(first) {
		t.Fatalf("older Hub: %s, %v; first %s", legacy, err, first)
	}
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1", n)
	}
	if rec, ok, err := e.st.GetResult(context.Background(), args.RequestID); err != nil || !ok || rec.CallerSession != "planner" {
		t.Fatalf("stored record = %+v, %v, %v", rec, ok, err)
	}
}
