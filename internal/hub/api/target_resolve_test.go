package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeResolver resolves "lab:<session>[:pane]" to a fixed Worker and records
// what it was asked.
type fakeResolver struct {
	wid   string
	calls []resolveCall
}

type resolveCall struct {
	p      authz.Principal
	target string
	all    bool
}

func (f *fakeResolver) Resolve(_ context.Context, p authz.Principal, target string, all bool) (ops.Target, error) {
	f.calls = append(f.calls, resolveCall{p, target, all})
	ht, err := ops.ParseHumanTarget(target)
	if err != nil {
		return ops.Target{}, err
	}
	if ht.WorkerName != "lab" {
		return ops.Target{}, proto.NewError(proto.CodeTargetNotFound, "no Worker of that name is visible to you")
	}
	return ht.Resolve(f.wid), nil
}

func TestHumanTargetIsResolvedForTheCallerBeforeTheCallIsDecided(t *testing.T) {
	r := newRig(t)
	wid := ops.NewUUIDv7()
	fr := &fakeResolver{wid: wid}
	if err := RegisterTargetResolution(r.reg, fr); err != nil {
		t.Fatal(err)
	}
	var got ops.SessionSnapshotArgs
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(_ context.Context, c *Call) (any, error) {
		got = *c.Args.(*ops.SessionSnapshotArgs)
		return ops.SessionSnapshotResult{}, nil
	})

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"body", "POST", "/api/v1/ops/session_snapshot", `{"target":"lab:minion-3:0.1"}`},
		{"query", "GET", "/api/v1/ops/session_snapshot?target=lab:minion-3:0.1", ""},
	} {
		got = ops.SessionSnapshotArgs{}
		fr.calls = nil
		n := r.decide.count()
		w := r.do(tc.method, tc.path, tc.body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if want := (ops.Target{WorkerID: wid, SessionName: "minion-3", Pane: "0.1"}); got.Target != want {
			t.Errorf("%s: target = %+v, want %+v", tc.name, got.Target, want)
		}
		if len(fr.calls) != 1 || fr.calls[0].p != testIdentity.Principal || fr.calls[0].all {
			t.Errorf("%s: resolver calls = %+v", tc.name, fr.calls)
		}
		last := r.decide.calls[len(r.decide.calls)-1]
		if r.decide.count() != n+1 || last.res != (authz.SessionResource{WorkerID: wid, Name: "minion-3"}) {
			t.Errorf("%s: decided on %#v", tc.name, last.res)
		}
	}
}

func TestHumanTargetAllFlagReachesTheResolver(t *testing.T) {
	r := newRig(t)
	fr := &fakeResolver{wid: ops.NewUUIDv7()}
	if err := RegisterTargetResolution(r.reg, fr); err != nil {
		t.Fatal(err)
	}
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(context.Context, *Call) (any, error) { return ops.SessionSnapshotResult{}, nil })
	if w := r.do("POST", "/api/v1/ops/session_snapshot", `{"target":"lab:minion-3","all":true}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := r.do("GET", "/api/v1/ops/session_snapshot?target=lab:minion-3&all=true", ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(fr.calls) != 2 || !fr.calls[0].all || !fr.calls[1].all {
		t.Errorf("calls = %+v", fr.calls)
	}
}

func TestHumanTargetErrorsStopTheCallBeforeDecide(t *testing.T) {
	r := newRig(t)
	if err := RegisterTargetResolution(r.reg, &fakeResolver{wid: ops.NewUUIDv7()}); err != nil {
		t.Fatal(err)
	}
	ran := false
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(context.Context, *Call) (any, error) { ran = true; return ops.SessionSnapshotResult{}, nil })
	for _, tc := range []struct {
		name, body string
		code       proto.Code
	}{
		{"unknown Worker", `{"target":"other:minion-3"}`, proto.CodeTargetNotFound},
		{"malformed", `{"target":"lab"}`, proto.CodeInvalidArgument},
		{"empty", `{"target":""}`, proto.CodeInvalidArgument},
	} {
		w := r.do("POST", "/api/v1/ops/session_snapshot", tc.body)
		if errCode(t, w) != tc.code {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
	}
	if ran || r.decide.count() != 0 {
		t.Errorf("handler ran = %v, decide calls = %d", ran, r.decide.count())
	}
}

func TestCanonicalTargetNeedsNoResolution(t *testing.T) {
	r := newRig(t)
	fr := &fakeResolver{wid: ops.NewUUIDv7()}
	if err := RegisterTargetResolution(r.reg, fr); err != nil {
		t.Fatal(err)
	}
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(context.Context, *Call) (any, error) { return ops.SessionSnapshotResult{}, nil })
	b, _ := json.Marshal(map[string]any{"target": ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "minion-1"}})
	if w := r.do("POST", "/api/v1/ops/session_snapshot", string(b)); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(fr.calls) != 0 {
		t.Errorf("resolver called for a canonical target: %+v", fr.calls)
	}
}

func TestHumanTargetWithoutResolverIsInvalid(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(context.Context, *Call) (any, error) { return ops.SessionSnapshotResult{}, nil })
	w := r.do("POST", "/api/v1/ops/session_snapshot", `{"target":"lab:minion-3"}`)
	if errCode(t, w) != proto.CodeInvalidArgument {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if err := RegisterTargetResolution(nil, nil); err == nil {
		t.Error("nil registry accepted")
	}
}

// codeResolver resolves the code "5-667" (with an optional pane) like a
// workers.Router with session codes.
type codeResolver struct {
	wid   string
	calls []string
}

func (f *codeResolver) Resolve(_ context.Context, _ authz.Principal, target string, _ bool) (ops.Target, error) {
	f.calls = append(f.calls, target)
	code, pane, ok := ops.ParseSessionCode(target)
	if !ok || code != (ops.SessionCode{Worker: 5, Session: 667}) {
		return ops.Target{}, proto.NewError(proto.CodeTargetNotFound, "no Worker of that name is visible to you")
	}
	return ops.Target{WorkerID: f.wid, SessionName: "minion-3", Pane: pane}, nil
}

func TestSessionCodeTargetObjectIsResolved(t *testing.T) {
	r := newRig(t)
	wid := ops.NewUUIDv7()
	fr := &codeResolver{wid: wid}
	if err := RegisterTargetResolution(r.reg, fr); err != nil {
		t.Fatal(err)
	}
	var got ops.SessionSnapshotArgs
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(_ context.Context, c *Call) (any, error) {
		got = *c.Args.(*ops.SessionSnapshotArgs)
		return ops.SessionSnapshotResult{}, nil
	})
	for _, tc := range []struct{ body, want string }{
		{`{"target":"5-667"}`, "5-667"},
		{`{"target":{"session_name":"5-667"}}`, "5-667"},
		{`{"target":{"session_name":"#5-667","pane":"0.1"}}`, "5-667:0.1"},
	} {
		fr.calls = nil
		w := r.do("POST", "/api/v1/ops/session_snapshot", tc.body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body)
		}
		if len(fr.calls) != 1 || fr.calls[0] != tc.want || got.Target.WorkerID != wid || got.Target.SessionName != "minion-3" {
			t.Errorf("%s: calls %q, target %+v", tc.body, fr.calls, got.Target)
		}
	}
	// An unknown code is the same not_found as an invisible Worker.
	if w := r.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"session_name":"5-668"}}`); w.Code != 404 {
		t.Fatalf("unknown code: %d %s", w.Code, w.Body)
	}
	// An object that names its Worker is never read as a code.
	fr.calls = nil
	body, _ := json.Marshal(map[string]any{"target": ops.Target{WorkerID: wid, SessionName: "5-667"}})
	if w := r.do("POST", "/api/v1/ops/session_snapshot", string(body)); w.Code != 200 || len(fr.calls) != 0 || got.Target.SessionName != "5-667" {
		t.Fatalf("canonical: %d calls %q target %+v", w.Code, fr.calls, got.Target)
	}
}
