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
