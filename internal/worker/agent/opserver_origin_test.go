package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The receiver side of F-01: a real executor behind a real op server, with the
// receiver policy pipe_accept_from, the Worker's id and its owner from welcome.

const (
	selfWorker  = "01a00000-0000-7000-8000-000000000001"
	peerWorker  = "01a00000-0000-7000-8000-000000000002"
	otherWorker = "01a00000-0000-7000-8000-000000000003"
	ownerA      = "user-a"
	ownerB      = "user-b"
)

type originEnv struct {
	*env
	srv *OpServer
}

func newOriginEnv(t *testing.T, accept string) *originEnv {
	t.Helper()
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "prompt")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	a, err := config.ParsePipeAcceptFrom(accept)
	if err != nil {
		t.Fatal(err)
	}
	e.policy.SetPipeAcceptFrom(a)
	e.policy.SetWorkerID(selfWorker)
	e.policy.SetOwnerID(ownerA)
	srv, err := NewOpServer(OpServerConfig{Executor: e.ex, Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	return &originEnv{env: e, srv: srv}
}

func workerOrigin(id, fp, owner string) *proto.Origin {
	return &proto.Origin{Kind: proto.OriginWorker, SourceWorkerID: id, SourceWorkerFP: fp, SourceWorkerOwnerID: owner}
}

// serve sends one op.request frame as the Hub would and returns the final frame
// type ("op.result" or "error") and the error code.
func (o *originEnv) serve(op string, args any, origin any) (string, proto.Code) {
	o.t.Helper()
	rq := map[string]any{"type": proto.TypeOpRequest, "op": op, "actions": []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage}, "args": args}
	if origin != nil {
		rq["origin"] = origin
	}
	st := newFakeOpStream(o.t, rq)
	o.srv.ServeStream(context.Background(), st)
	types := st.types(o.t)
	last := types[len(types)-1]
	if last == proto.TypeError {
		return last, st.errorFrame(o.t).Code
	}
	return last, ""
}

func messageArgs(op string) any {
	switch op {
	case ops.OpPipeSend:
		a := pipeSendArgs("work", "hi", false)
		a.MessageID = 7
		return a
	case ops.OpSendKeys:
		return sendKeys("work", false, "Enter")
	case ops.OpSetTask:
		return ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Task: "t"}
	case ops.OpAnswer:
		return ops.AnswerArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Choice: "1"}
	}
	return sendText("work", "hi", false)
}

var messageOps = []string{ops.OpPipeSend, ops.OpSendText, ops.OpSendKeys, ops.OpAnswer, ops.OpSetTask}

func TestMessageClassOpsAreRefusedWithoutAValidOrigin(t *testing.T) {
	o := newOriginEnv(t, "owner-workers")
	for _, op := range messageOps {
		t.Run(op+" without origin", func(t *testing.T) {
			if typ, code := o.serve(op, messageArgs(op), nil); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
				t.Fatalf("%s: %s %s, want pipe_origin_refused", op, typ, code)
			}
		})
		t.Run(op+" malformed origin", func(t *testing.T) {
			bad := []any{
				map[string]any{},                                 // no kind
				map[string]any{"kind": "worker"},                 // no source
				map[string]any{"kind": "teapot", "user_id": "u"}, // unknown kind
				map[string]any{"kind": "worker", "source_worker_id": peerWorker, "source_worker_fp": "x"}, // partial
				"worker:" + peerWorker, // wrong JSON type
			}
			for _, b := range bad {
				if typ, code := o.serve(op, messageArgs(op), b); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
					t.Fatalf("%s %v: %s %s, want pipe_origin_refused", op, b, typ, code)
				}
			}
		})
	}
	if n := len(o.host.pastes()); n != 0 || len(o.host.keys()) != 0 {
		t.Fatalf("a refused request reached the pane: %d pastes, %d keys", n, len(o.host.keys()))
	}
	// Every refusal is in the local audit, with a usable origin marker.
	rows := o.audit()
	if len(rows) == 0 {
		t.Fatal("refusals left no audit row")
	}
	for _, r := range rows {
		if r.Reason != string(proto.CodePipeOriginRefused) || (r.Origin != "missing" && r.Origin != "malformed") {
			t.Fatalf("audit row %+v", r)
		}
	}
}

// Round 2: every Hub op.request needs a valid origin, not only message ops, and
// pipe_accept_from does not apply to the others. A Hub request is never audited
// as "local".
func TestEveryHubOpNeedsAValidOrigin(t *testing.T) {
	o := newOriginEnv(t, "none")
	cases := map[string]any{
		ops.OpSessionsList:  ops.SessionsListArgs{},
		ops.OpSessionCreate: ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: "fresh"},
		ops.OpShareSet:      ops.ShareSetArgs{WorkerID: selfWorker, Mode: "none"},
	}
	for op, args := range cases {
		if typ, code := o.serve(op, args, nil); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
			t.Fatalf("%s without origin: %s %s, want pipe_origin_refused", op, typ, code)
		}
		if typ, code := o.serve(op, args, map[string]any{"kind": "worker"}); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
			t.Fatalf("%s with a malformed origin: %s %s", op, typ, code)
		}
	}
	if o.host.creates != 0 {
		t.Fatal("session_create without origin created a session")
	}
	if o.saved().Mode == "none" {
		t.Fatal("share_set without origin changed the sharing")
	}
	// pipe_accept_from=none does not touch non-message ops from a peer Worker.
	peer := workerOrigin(peerWorker, "fp-peer", ownerA)
	if typ, code := o.serve(ops.OpSessionsList, ops.SessionsListArgs{}, peer); typ != proto.TypeOpResult {
		t.Fatalf("sessions_list from a peer Worker origin: %s %s", typ, code)
	}
	user := &proto.Origin{Kind: proto.OriginUser, UserID: ownerA}
	if typ, code := o.serve(ops.OpSessionsList, ops.SessionsListArgs{}, user); typ != proto.TypeOpResult {
		t.Fatalf("sessions_list with a user origin: %s %s", typ, code)
	}
	for _, r := range o.audit() {
		if r.Origin == "local" {
			t.Fatalf("a Hub request was audited as local: %+v", r)
		}
	}
}

func TestPipeAcceptFromNoneRefusesOtherWorkersButNotUsersOrItself(t *testing.T) {
	o := newOriginEnv(t, "none")
	for _, op := range messageOps {
		// Another Worker of the same owner, as a Worker identity and as a bound key.
		peer := workerOrigin(peerWorker, "fp-peer", ownerA)
		keyed := &proto.Origin{Kind: proto.OriginAPIKey, UserID: ownerA, KeyID: "k1", KeyBoundWorkerID: peerWorker,
			SourceWorkerID: peerWorker, SourceWorkerFP: "fp-peer", SourceWorkerOwnerID: ownerA}
		for name, origin := range map[string]*proto.Origin{"worker": peer, "bound key": keyed} {
			if typ, code := o.serve(op, messageArgs(op), origin); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
				t.Fatalf("%s from %s: %s %s, want pipe_origin_refused", op, name, typ, code)
			}
		}
		// A user and an unbound key are the Hub's own decision; this Worker
		// itself ("A -> itself") is always accepted.
		for name, origin := range map[string]*proto.Origin{
			"user":      {Kind: proto.OriginUser, UserID: ownerA},
			"act-as":    {Kind: proto.OriginUser, UserID: ownerA, ActorID: "admin"},
			"free key":  {Kind: proto.OriginAPIKey, UserID: ownerA, KeyID: "k2"},
			"self":      workerOrigin(selfWorker, "fp-self", ownerA),
			"self, key": {Kind: proto.OriginAPIKey, UserID: ownerB, KeyID: "k3", KeyBoundWorkerID: selfWorker, SourceWorkerID: selfWorker, SourceWorkerFP: "fp-self", SourceWorkerOwnerID: ownerA},
		} {
			// Accepted by the origin gate; the op itself may still end in its own
			// outcome or error (answer needs a dialog).
			if _, code := o.serve(op, messageArgs(op), origin); code == proto.CodePipeOriginRefused {
				t.Fatalf("%s from %s was refused for its origin", op, name)
			}
		}
	}
	// The refusals are audited with the origin.
	var refused int
	for _, r := range o.audit() {
		if r.Reason == string(proto.CodePipeOriginRefused) {
			refused++
			var got proto.Origin
			if json.Unmarshal([]byte(r.Origin), &got) != nil || got.SourceWorkerID != peerWorker {
				t.Fatalf("refusal audited with origin %q", r.Origin)
			}
		}
	}
	if refused != 2*len(messageOps) {
		t.Fatalf("%d refusals audited, want %d", refused, 2*len(messageOps))
	}
}

func TestPipeAcceptFromOwnerWorkersComparesOwnerIDs(t *testing.T) {
	o := newOriginEnv(t, "owner-workers")
	// Same owner: accepted. Another owner: refused.
	if typ, code := o.serve(ops.OpPipeSend, messageArgs(ops.OpPipeSend), workerOrigin(peerWorker, "fp-peer", ownerA)); typ != proto.TypeOpResult {
		t.Fatalf("same owner: %s %s", typ, code)
	}
	if typ, code := o.serve(ops.OpPipeSend, messageArgs(ops.OpPipeSend), workerOrigin(otherWorker, "fp-other", ownerB)); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
		t.Fatalf("other owner: %s %s", typ, code)
	}
	// N5: a key owned by someone else but bound to a Worker of the receiver's
	// owner is judged by the owner of the bound Worker, not by the key owner...
	keyOfB := &proto.Origin{Kind: proto.OriginAPIKey, UserID: ownerB, KeyID: "k", KeyBoundWorkerID: peerWorker,
		SourceWorkerID: peerWorker, SourceWorkerFP: "fp-peer", SourceWorkerOwnerID: ownerA}
	if typ, code := o.serve(ops.OpPipeSend, messageArgs(ops.OpPipeSend), keyOfB); typ != proto.TypeOpResult {
		t.Fatalf("key of another user bound to a same-owner Worker: %s %s", typ, code)
	}
	// ...and a key of the receiver's owner bound to a Worker of someone else is refused.
	keyOfA := &proto.Origin{Kind: proto.OriginAPIKey, UserID: ownerA, KeyID: "k", KeyBoundWorkerID: otherWorker,
		SourceWorkerID: otherWorker, SourceWorkerFP: "fp-other", SourceWorkerOwnerID: ownerB}
	if typ, code := o.serve(ops.OpPipeSend, messageArgs(ops.OpPipeSend), keyOfA); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
		t.Fatalf("key of the owner bound to a foreign Worker: %s %s", typ, code)
	}
	// Without a welcome the Worker knows no owner: nobody matches, not even an
	// empty owner id (which a valid origin cannot have anyway).
	o.policy.SetOwnerID("")
	if typ, code := o.serve(ops.OpPipeSend, messageArgs(ops.OpPipeSend), workerOrigin(peerWorker, "fp-peer", ownerA)); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
		t.Fatalf("no owner known: %s %s", typ, code)
	}
}

func TestPipeAcceptFromListNamesIdsAndFingerprints(t *testing.T) {
	fp := strings.Repeat("a", 52)
	o := newOriginEnv(t, peerWorker+", "+strings.ToUpper(fp))
	cases := map[string]struct {
		origin *proto.Origin
		ok     bool
	}{
		"listed id":          {workerOrigin(peerWorker, "fp-1", ownerA), true},
		"listed fingerprint": {workerOrigin(otherWorker, fp, ownerB), true},
		"unlisted":           {workerOrigin(otherWorker, "fp-3", ownerA), false},
	}
	for name, c := range cases {
		typ, code := o.serve(ops.OpSendText, messageArgs(ops.OpSendText), c.origin)
		if c.ok != (typ == proto.TypeOpResult) || (!c.ok && code != proto.CodePipeOriginRefused) {
			t.Fatalf("%s: %s %s", name, typ, code)
		}
	}
}

// pipe_accept_from is a local setting: no Hub request can change it, and an
// origin cannot be smuggled in the arguments.
func TestHubOpsCannotChangePipeAcceptFromOrForgeAnOrigin(t *testing.T) {
	o := newOriginEnv(t, "none")
	before := o.policy.PipeAcceptFrom()
	// Every Worker op the Hub can route, with a forged setting in its arguments.
	forged := map[string]any{"pipe_accept_from": "owner-workers", "accept": "owner-workers", "request_id": ops.NewUUIDv7()}
	user := &proto.Origin{Kind: proto.OriginUser, UserID: ownerA}
	for _, op := range ops.Default.All() {
		if op.Scope != ops.ScopeWorker {
			continue
		}
		_, _ = o.serve(op.Name, forged, user)
	}
	if got := o.policy.PipeAcceptFrom(); got.String() != before.String() || got.OwnerWorkers {
		t.Fatalf("a Hub op changed pipe_accept_from: %+v", got)
	}
	// The origin lives in the frame; one in the arguments is ignored, so it
	// cannot turn a peer Worker into a user or into this Worker.
	args := pipeSendArgs("work", "hi", false)
	args.MessageID = 7
	raw := mustJSON(t, args)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["origin"] = map[string]any{"kind": "user", "user_id": ownerA}
	if typ, code := o.serve(ops.OpPipeSend, m, workerOrigin(peerWorker, "fp-peer", ownerA)); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
		t.Fatalf("forged origin in the arguments: %s %s", typ, code)
	}
	// The executor does not take it either: an origin in the arguments is not
	// the Request's origin, so the audit says "local".
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	if _, err := e.ex.Execute(context.Background(), Request{Op: ops.OpSendText, Actions: allActions, Args: mustJSON(t, map[string]any{
		"request_id": ops.NewUUIDv7(), "target": target("work"), "text": "x", "origin": map[string]any{"kind": "user", "user_id": "x"},
	})}); err != nil {
		t.Fatal(err)
	}
	if rows := e.audit(); len(rows) != 1 || rows[0].Origin != "local" {
		t.Fatalf("audit = %+v, want origin local", rows)
	}
}

// Delivered Hub writes are audited with the origin that reached the Worker.
func TestWorkerAuditRecordsTheOriginOfHubWrites(t *testing.T) {
	o := newOriginEnv(t, "owner-workers")
	origin := workerOrigin(peerWorker, "fp-peer", ownerA)
	if typ, code := o.serve(ops.OpSendText, messageArgs(ops.OpSendText), origin); typ != proto.TypeOpResult {
		t.Fatalf("%s %s", typ, code)
	}
	rows := o.audit()
	if len(rows) != 1 || rows[0].Op != ops.OpSendText {
		t.Fatalf("audit = %+v", rows)
	}
	var got proto.Origin
	if err := json.Unmarshal([]byte(rows[0].Origin), &got); err != nil || got != *origin {
		t.Fatalf("audit origin = %q (%v), want %+v", rows[0].Origin, err, origin)
	}
}
