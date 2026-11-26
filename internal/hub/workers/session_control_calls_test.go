package workers

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeGrants is a session_control table in memory.
type fakeGrants struct {
	mu   sync.Mutex
	rows map[[2]string]string // (worker, session) -> targets JSON
	// direct are the session grants by (source worker, source session).
	direct map[[2]string][]store.SessionGrantRow
	// directErr, when set, fails every read of the direct grants.
	directErr error
}

func (g *fakeGrants) failDirect(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.directErr = err
}

func (g *fakeGrants) SessionGrantsFor(_ context.Context, workerID, session string) ([]store.SessionGrantRow, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.directErr != nil {
		return nil, g.directErr
	}
	return append([]store.SessionGrantRow(nil), g.direct[[2]string{workerID, session}]...), nil
}

// setDirect replaces the direct grants of a source session.
func (g *fakeGrants) setDirect(workerID, session string, rows ...store.SessionGrantRow) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.direct == nil {
		g.direct = map[[2]string][]store.SessionGrantRow{}
	}
	g.direct[[2]string{workerID, session}] = rows
}

func (g *fakeGrants) SessionControl(_ context.Context, workerID, session string) (store.SessionControlRow, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.rows[[2]string{workerID, session}]
	if !ok {
		return store.SessionControlRow{}, store.ErrNotFound
	}
	return store.SessionControlRow{WorkerID: workerID, SessionName: session, TargetsJSON: t}, nil
}

func (g *fakeGrants) set(t *testing.T, workerID, session string, targets ...ops.KeyResource) {
	t.Helper()
	b, err := json.Marshal(targets)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rows == nil {
		g.rows = map[[2]string]string{}
	}
	g.rows[[2]string{workerID, session}] = string(b)
}

func (g *fakeGrants) remove(workerID, session string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.rows, [2]string{workerID, session})
}

// scRig is a call server with the session-control ops on a real Router.
type scRig struct {
	*rig
	cs     *CallServer
	grants *fakeGrants
	audit  *memAudit
}

func newSCRig(t *testing.T) *scRig {
	t.Helper()
	r := newRig(t, nil)
	g := &fakeGrants{}
	au := &memAudit{}
	cs := NewCallServer(CallServerConfig{
		Keys: newFakeKeys(), Resolver: r.rt, Owners: r.dir, Auditor: au, Grants: g,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err := RegisterSessionControlCalls(cs, r.rt); err != nil {
		t.Fatal(err)
	}
	return &scRig{rig: r, cs: cs, grants: g, audit: au}
}

// call serves one call stream of workerID; the Worker answers with answer.
func (r *scRig) call(t *testing.T, workerID string, req map[string]any, answer any) (frames []json.RawMessage, sent *fakeStream) {
	t.Helper()
	sent = newFakeStream(resultFrame(t, answer))
	r.script(sent)
	st := newCallStream(t, req)
	r.cs.Serve(context.Background(), workerID, st)
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]json.RawMessage(nil), st.written...), sent
}

func scRequest(op string, args any, key, source string) map[string]any {
	m := request(op, args, key)
	if source != "" {
		m["source_session"] = source
	}
	return m
}

func resultOf(t *testing.T, frames []json.RawMessage, into any) {
	t.Helper()
	typ, body := lastFrame(t, frames)
	if typ != proto.TypeOpResult {
		t.Fatalf("answer = %s, want a result", body)
	}
	var f struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &f); err != nil || json.Unmarshal(f.Result, into) != nil {
		t.Fatalf("answer = %s", body)
	}
}

var (
	gAlicePi2 = ops.KeyResource{Kind: ops.KeyResourceWorker, WorkerID: wAlicePi2.ID}
	gBobBuild = ops.KeyResource{Kind: ops.KeyResourceSession, WorkerID: wBobPi1.ID, SessionName: "build"}
)

func scArgs(op string, target any) map[string]any {
	switch op {
	case ops.OpSessionsList:
		if tg, ok := target.(ops.Target); ok {
			return map[string]any{"worker_id": tg.WorkerID}
		}
		return map[string]any{}
	case ops.OpWorkersList:
		return map[string]any{}
	case ops.OpSendText:
		return map[string]any{"request_id": ops.NewUUIDv7(), "target": target, "text": "hi"}
	case ops.OpSendKeys:
		return map[string]any{"request_id": ops.NewUUIDv7(), "target": target, "keys": []string{"Enter"}}
	case ops.OpSetTask:
		return map[string]any{"request_id": ops.NewUUIDv7(), "target": target, "task": "t"}
	case ops.OpAnswer:
		return map[string]any{"request_id": ops.NewUUIDv7(), "target": target, "choice": "1"}
	}
	return map[string]any{"target": target}
}

// The new ops are served only to a caller with a grant (Astra F1): a plain
// Worker identity, a session without a grant row and a call key are refused
// with forbidden, and nothing reaches a Worker.
func TestSessionControlOpsAreGrantOnly(t *testing.T) {
	r := newSCRig(t)
	r.grants.set(t, wAlicePi2.ID, "boss", gAlicePi2) // another Worker's grant does not count
	tgt := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	callers := []struct{ name, key, source string }{
		{"Worker identity", "", ""},
		{"session without a grant", "", "boss"},
		{"call key", keyBoundPi1, ""},
		{"call key and a session without a grant", keyBoundPi1, "other"},
	}
	for _, c := range callers {
		for _, op := range append(append([]string(nil), SessionControlOps...), ops.OpWorkersList) {
			frames, _ := r.call(t, wAlicePi1.ID, scRequest(op, scArgs(op, tgt), c.key, c.source), sessionsResult())
			if got := errCodeOf(t, frames); got != proto.CodeForbidden {
				t.Errorf("%s, %s: %q, want forbidden", c.name, op, got)
			}
		}
	}
	if n := r.be.openCount(); n != 0 {
		t.Fatalf("%d op streams were opened", n)
	}
}

// A granted session reads and types into its targets, on other owners'
// Workers too (admin-owned controller, all=true resolution), and nothing else.
func TestSessionControlGrantedSessionReachesItsTargetsOnly(t *testing.T) {
	r := newSCRig(t)
	r.grants.set(t, wRootLab.ID, "boss", gAlicePi2, gBobBuild)

	// Snapshot of a session on alice's pi-2 (Worker target), canonical form.
	frames, sent := r.call(t, wRootLab.ID, scRequest(ops.OpSessionSnapshot,
		scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "x"}), "", "boss"), map[string]any{})
	var snap ops.SessionSnapshotResult
	resultOf(t, frames, &snap)
	req := sent.request(t)
	want := proto.Origin{Kind: proto.OriginWorker, SourceWorkerID: wRootLab.ID, SourceWorkerFP: wRootLab.Fingerprint,
		SourceWorkerOwnerID: idRoot, Grant: true, SourceSession: "boss", SourceProof: proto.OriginProofClaimed}
	if req.Origin != want {
		t.Fatalf("origin = %+v, want %+v", req.Origin, want)
	}
	if req.TargetWorkerID != wAlicePi2.ID {
		t.Fatalf("target_worker_id = %q, want the routed Worker", req.TargetWorkerID)
	}

	// send_text on bob's pi-1:build, by the human form: resolved among all
	// Workers the grant reaches (alice's pi-1 is not in scope, so no conflict).
	frames, sent = r.call(t, wRootLab.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, "pi-1:build"), "", "boss"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	if wr.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text = %+v", wr)
	}
	if req := sent.request(t); req.Op != ops.OpSendText || !req.Origin.Grant {
		t.Fatalf("request = %+v", req)
	}
	evs := r.audit.all()
	if len(evs) != 1 || evs[0].Action != ops.OpSendText || evs[0].Target != wBobPi1.ID+":build" ||
		evs[0].Actor != "worker:"+wRootLab.ID || evs[0].Outcome != "success" {
		t.Fatalf("audit = %+v", evs)
	}
	// The row names the calling session, not only its Worker.
	if evs[0].Source != wRootLab.ID+":boss" {
		t.Fatalf("audit source = %q, want %q", evs[0].Source, wRootLab.ID+":boss")
	}

	// Out of scope: another session of bob's pi-1, bob's box, alice's pi-1.
	opens := r.be.openCount()
	for _, tgt := range []any{
		ops.Target{WorkerID: wBobPi1.ID, SessionName: "other"},
		ops.Target{WorkerID: wBobBox.ID, SessionName: "build"},
		ops.Target{WorkerID: wAlicePi1.ID, SessionName: "build"},
		"box:build",
	} {
		for _, op := range []string{ops.OpSessionSnapshot, ops.OpSendText} {
			frames, _ := r.call(t, wRootLab.ID, scRequest(op, scArgs(op, tgt), "", "boss"), map[string]any{})
			if c := errCodeOf(t, frames); c != proto.CodeNotFound && c != proto.CodeForbidden && c != proto.CodeTargetNotFound {
				t.Errorf("%s on %v: %q, want a refusal", op, tgt, c)
			}
		}
	}
	if r.be.openCount() != opens {
		t.Fatal("an out-of-scope request reached a Worker")
	}
}

// The grant wins over a call key: the call runs as the grant principal and
// the origin is the Worker identity's, marked grant, never an apikey origin.
func TestSessionControlGrantWinsOverTheCallKey(t *testing.T) {
	r := newSCRig(t)
	r.grants.set(t, wAlicePi1.ID, "boss", gAlicePi2)
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionSnapshot,
		scArgs(ops.OpSessionSnapshot, "pi-2:x"), keyBoundPi1, "boss"), map[string]any{})
	var snap ops.SessionSnapshotResult
	resultOf(t, frames, &snap)
	o := sent.request(t).Origin
	if o.Kind != proto.OriginWorker || !o.Grant || o.KeyID != "" || o.SourceWorkerID != wAlicePi1.ID {
		t.Fatalf("origin = %+v", o)
	}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Pipe ops keep their behavior: with a key and no grant the key is the
// caller; with a grant on the source session the grant principal is.
func TestSessionControlPipeWithKeyUnchangedAndGrantWins(t *testing.T) {
	g := &fakeGrants{}
	r := newCallRig(t, func(c *CallServerConfig) { c.Grants = g })
	send := func(source string) authz.Principal {
		t.Helper()
		n := r.rt.count()
		frames := r.call(t, wAlicePi1.ID, scRequest(ops.OpPipeSend,
			map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-2:minion-1", "text": "x"}, keyBoundPi1, source))
		if typ, body := lastFrame(t, frames); typ != proto.TypeOpResult {
			t.Fatalf("pipe_send = %s", body)
		}
		if r.rt.count() != n+1 {
			t.Fatal("pipe_send was not routed")
		}
		return r.rt.calls[n].Principal
	}
	if p := send(""); p.Kind != authz.KindAPIKey {
		t.Fatalf("without source_session: routed as %q", p.Kind)
	}
	if p := send("boss"); p.Kind != authz.KindAPIKey {
		t.Fatalf("source_session without a grant: routed as %q", p.Kind)
	}
	g.set(t, wAlicePi1.ID, "boss", gAlicePi2)
	if p := send("boss"); p.Kind != authz.KindWorker || p.Scope == nil || p.WorkerID != wAlicePi1.ID {
		t.Fatalf("with a grant: routed as %+v", p)
	}
}

// A removed grant is refused on the next call.
func TestSessionControlRemovedGrantIsRefusedOnTheNextCall(t *testing.T) {
	r := newSCRig(t)
	r.grants.set(t, wRootLab.ID, "boss", gAlicePi2)
	args := scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "x"})
	frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpSessionSnapshot, args, "", "boss"), map[string]any{})
	var snap ops.SessionSnapshotResult
	resultOf(t, frames, &snap)
	r.grants.remove(wRootLab.ID, "boss")
	frames, _ = r.call(t, wRootLab.ID, scRequest(ops.OpSessionSnapshot, args, "", "boss"), map[string]any{})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("after removal: %q, want forbidden", c)
	}
}

// A running call ends on the re-check after its grant was removed or changed.
func TestSessionControlRecheckEndsARunningCall(t *testing.T) {
	for _, change := range []string{"removed", "changed"} {
		t.Run(change, func(t *testing.T) {
			g := &fakeGrants{}
			g.set(t, wRootLab.ID, "boss", gAlicePi2)
			cs := NewCallServer(CallServerConfig{Grants: g, ReauthInterval: 5 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
			_, _, grant, err := cs.authenticate(context.Background(), wRootLab.ID, "", "boss", ops.OpSendText)
			if err != nil || grant == "" {
				t.Fatalf("grant %q %v", grant, err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan struct{})
			go func() {
				defer close(done)
				cs.reauthorizeWorker(ctx, cancel, authz.NewWorkerPrincipal(wRootLab.ID), "boss", grant, true)
			}()
			time.Sleep(20 * time.Millisecond)
			if ctx.Err() != nil {
				t.Fatal("ended while the grant was unchanged")
			}
			if change == "removed" {
				g.remove(wRootLab.ID, "boss")
			} else {
				g.set(t, wRootLab.ID, "boss", gBobBuild)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the call was not withdrawn")
			}
			if c := proto.CodeOf(context.Cause(ctx)); c != proto.CodeForbidden {
				t.Fatalf("cause = %v", context.Cause(ctx))
			}
		})
	}
}

// workers_list and sessions_list are filtered by the grant's scope.
func TestSessionControlListsAreFilteredByTheGrant(t *testing.T) {
	r := newSCRig(t)
	r.grants.set(t, wRootLab.ID, "boss", gAlicePi2, gBobBuild)

	frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpWorkersList, map[string]any{"all": false}, "", "boss"), nil)
	var wl ops.WorkersListResult
	resultOf(t, frames, &wl)
	got := map[string]bool{}
	for _, w := range wl.Workers {
		got[w.ID] = true
	}
	// alice's pi-2 (in scope) and the controller's own Worker (own-Worker
	// rule); bob's pi-1 is reached only through one session, as for a key.
	if len(got) != 2 || !got[wAlicePi2.ID] || !got[wRootLab.ID] {
		t.Fatalf("workers_list = %+v", wl.Workers)
	}

	frames, sent := r.call(t, wRootLab.ID, scRequest(ops.OpSessionsList, map[string]any{"worker_id": wAlicePi2.ID}, "", "boss"), sessionsResult())
	var sl ops.SessionsListResult
	resultOf(t, frames, &sl)
	if len(sl.Sessions) != 1 || !sent.request(t).Origin.Grant {
		t.Fatalf("sessions_list = %+v", sl)
	}
	for _, args := range []map[string]any{{"worker_id": wBobBox.ID}, {"worker_id": wAlicePi1.ID}} {
		frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpSessionsList, args, "", "boss"), sessionsResult())
		if c := errCodeOf(t, frames); c != proto.CodeNotFound && c != proto.CodeForbidden {
			t.Errorf("sessions_list %v: %q, want a refusal", args, c)
		}
	}
	for _, args := range []map[string]any{{}, {"worker_id": "pi-2"}} {
		frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpSessionsList, args, "", "boss"), sessionsResult())
		if c := errCodeOf(t, frames); c != proto.CodeInvalidArgument {
			t.Errorf("sessions_list %v: %q, want invalid_argument", args, c)
		}
	}
}

// origin.grant is set only when the grant authorized the request: a granted
// session acting on its own Worker outside the grant is authorized by the
// own-Worker rule and is not marked.
func TestSessionControlOriginGrantOnlyOnGrantDecisions(t *testing.T) {
	r := newSCRig(t)
	r.grants.set(t, wAlicePi1.ID, "boss", gAlicePi2)
	_, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionSnapshot,
		scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}), "", "boss"), map[string]any{})
	if o := sent.request(t).Origin; o.Grant || o.Kind != proto.OriginWorker {
		t.Fatalf("own Worker: origin = %+v", o)
	}
	_, sent = r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionSnapshot,
		scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "minion-1"}), "", "boss"), map[string]any{})
	if o := sent.request(t).Origin; !o.Grant {
		t.Fatalf("granted target: origin = %+v", o)
	}
}

// An invalid source_session is refused before anything else.
func TestSessionControlInvalidSourceSessionIsRefused(t *testing.T) {
	r := newSCRig(t)
	for _, bad := range []string{"a:b", "a.b", " lead", "\x01"} {
		frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpWorkersList, map[string]any{}, "", bad), nil)
		if c := errCodeOf(t, frames); c != proto.CodeInvalidArgument {
			t.Errorf("source_session %q: %q, want invalid_argument", bad, c)
		}
	}
}
