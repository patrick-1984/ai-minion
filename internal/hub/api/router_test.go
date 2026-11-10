package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestUnknownAPIPathsAreEnvelopedNotFound(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/nothing"}, {"POST", "/api/v1/"}, {"GET", "/api/v1/ops/"}, {"GET", "/api/v1/ops/a/b"}, {"DELETE", "/api/v1/x/y/z"},
	} {
		w := r.do(c.method, c.path, "")
		if errCode(t, w) != proto.CodeNotFound {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if r.authn.n != 0 {
		t.Error("an unknown route authenticated the caller")
	}
}

func TestMoreSpecificRoutesOfOtherModulesWin(t *testing.T) {
	r := newRig(t)
	r.mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("sse")) })
	if w := r.do("GET", "/api/v1/events", ""); w.Body.String() != "sse" {
		t.Errorf("body = %q", w.Body)
	}
}

func TestNewRouterNeedsItsParts(t *testing.T) {
	if _, err := NewRouter(&Dispatcher{}, http.NewServeMux()); err == nil {
		t.Error("an incomplete dispatcher was accepted")
	}
	r := newRig(t)
	// Mounting twice on one mux conflicts and is an error, not a panic.
	if _, err := NewRouter(r.d, r.mux); err == nil {
		t.Error("a second router on the same mux was accepted")
	}
}

func TestAliasGetRunsTheSamePipeline(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPing, echoHandler)
	if err := r.router.AddAlias(Alias{Method: "GET", Pattern: "/api/v1/ping", Op: opPing}); err != nil {
		t.Fatal(err)
	}
	w := r.do("GET", "/api/v1/ping?message=alias", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"echo":"alias"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r.decide.count() != 1 || !r.decide.calls[0].accepted.Permits(ops.ActionManage) {
		t.Error("the alias skipped authz.Decide")
	}
	// The alias fixes the method; another method falls through to the
	// enveloped not_found.
	if w := r.do("POST", "/api/v1/ping", `{}`); errCode(t, w) != proto.CodeNotFound {
		t.Errorf("POST on a GET alias = %d %s", w.Code, w.Body)
	}
}

func TestAliasPathParametersFillArgumentsAndOverrideTheBody(t *testing.T) {
	r := newRig(t)
	var got *shareSetArgs
	mustHandle(t, r.reg, opShareSet, func(_ context.Context, c *Call) (any, error) {
		got = c.Args.(*shareSetArgs)
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
	})
	if err := r.router.AddAlias(Alias{Method: "PUT", Pattern: "/api/v1/workers/{worker_id}/share", Op: opShareSet}); err != nil {
		t.Fatal(err)
	}
	wid := ops.NewUUIDv7()
	w := r.do("PUT", "/api/v1/workers/"+wid+"/share", `{"request_id":"`+ops.NewUUIDv7()+`","mode":"all","worker_id":"body-must-lose"}`)
	if w.Code != 200 || got == nil || got.WorkerID != wid || got.Mode != "all" {
		t.Fatalf("%d %s args=%+v", w.Code, w.Body, got)
	}
	if res := r.decide.calls[0]; ResourceLabel(res.res) != wid || !res.accepted.Permits(ops.ActionOperate) {
		t.Errorf("Decide(%+v)", res)
	}
	if w := r.do("GET", "/api/v1/workers/"+wid+"/share", ""); w.Code == 200 {
		t.Error("GET reached a PUT alias")
	}
}

func TestAliasParamsMapToNestedMembers(t *testing.T) {
	r := newRig(t)
	var got *ops.SessionSnapshotArgs
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(_ context.Context, c *Call) (any, error) {
		got = c.Args.(*ops.SessionSnapshotArgs)
		return ops.SessionSnapshotResult{}, nil
	})
	err := r.router.AddAlias(Alias{
		Method: "GET", Pattern: "/api/v1/workers/{wid}/sessions/{name}/screen", Op: ops.OpSessionSnapshot,
		Params: map[string]string{"wid": "target.worker_id", "name": "target.session_name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wid := ops.NewUUIDv7()
	w := r.do("GET", "/api/v1/workers/"+wid+"/sessions/minion-3/screen", "")
	if w.Code != 200 || got.Target.WorkerID != wid || got.Target.SessionName != "minion-3" {
		t.Fatalf("%d %s target=%+v", w.Code, w.Body, got)
	}
}

func TestAliasPathParameterWithBadTypeIsInvalid(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPing, echoHandler)
	if err := r.router.AddAlias(Alias{Method: "GET", Pattern: "/api/v1/ping/{limit}", Op: opPing}); err != nil {
		t.Fatal(err)
	}
	if w := r.do("GET", "/api/v1/ping/abc", ""); errCode(t, w) != proto.CodeInvalidArgument {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestAddAliasValidation(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		name string
		a    Alias
		want string
	}{
		{"unknown op", Alias{Method: "GET", Pattern: "/api/v1/a", Op: "nope"}, "not an operation"},
		{"GET on a write", Alias{Method: "GET", Pattern: "/api/v1/a", Op: opPingSet}, "write operation"},
		{"DELETE on a read", Alias{Method: "DELETE", Pattern: "/api/v1/a", Op: opPing}, "read operation"},
		{"bad method", Alias{Method: "TRACE", Pattern: "/api/v1/a", Op: opPing}, "unsupported method"},
		{"outside the prefix", Alias{Method: "GET", Pattern: "/other", Op: opPing}, "must be under"},
		{"inside ops", Alias{Method: "GET", Pattern: "/api/v1/ops/x", Op: opPing}, "must be under"},
		{"wildcard fills nothing", Alias{Method: "GET", Pattern: "/api/v1/a/{nothing}", Op: opPing}, "not an argument"},
		{"param for a missing wildcard", Alias{Method: "GET", Pattern: "/api/v1/a", Op: opPing, Params: map[string]string{"x": "message"}}, "not in the pattern"},
		{"ok", Alias{Method: "GET", Pattern: "/api/v1/ok", Op: opPing}, ""},
		{"conflict with a registered route", Alias{Method: "GET", Pattern: "/api/v1/ok", Op: opPing}, "cannot register"},
	}
	for _, c := range cases {
		err := r.router.AddAlias(c.a)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// A body member that the standard decoder would match to worker_id by case or
// Unicode folding (the Kelvin sign folds to "k") must not override the path
// parameter: the call acts on the Worker its URL names.
func TestAliasPathParameterBeatsFoldedBodyMembers(t *testing.T) {
	r := newRig(t)
	var got *shareSetArgs
	mustHandle(t, r.reg, opShareSet, func(_ context.Context, c *Call) (any, error) {
		got = c.Args.(*shareSetArgs)
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
	})
	if err := r.router.AddAlias(Alias{Method: "PUT", Pattern: "/api/v1/workers/{worker_id}/share", Op: opShareSet}); err != nil {
		t.Fatal(err)
	}
	wid, other := ops.NewUUIDv7(), ops.NewUUIDv7()
	for _, key := range []string{"wor\u212Aer_id", "WORKER_ID", "Worker_Id"} {
		got = nil
		before := r.decide.count()
		w := r.do("PUT", "/api/v1/workers/"+wid+"/share", `{"request_id":"`+ops.NewUUIDv7()+`","mode":"all","`+key+`":"`+other+`"}`)
		if w.Code != 200 || got == nil || got.WorkerID != wid {
			t.Fatalf("%q: %d %s args=%+v", key, w.Code, w.Body, got)
		}
		if res := r.decide.calls[before]; ResourceLabel(res.res) != wid {
			t.Errorf("%q: decided on %v", key, res.res)
		}
	}
}

// sessions_list and session_create route by a worker_id that their argument
// structs do not declare; the alias supplies it from the path.
func TestAliasRoutesWorkerOpsWithoutAWorkerArgument(t *testing.T) {
	r := newRig(t)
	var got *Call
	h := func(_ context.Context, c *Call) (any, error) { got = c; return struct{}{}, nil }
	mustHandle(t, r.reg, ops.OpSessionsList, h)
	mustHandle(t, r.reg, ops.OpSessionCreate, h)
	if err := r.router.AddAlias(Alias{Method: "GET", Pattern: "/api/v1/workers/{worker_id}/sessions", Op: ops.OpSessionsList}); err != nil {
		t.Fatal(err)
	}
	if err := r.router.AddAlias(Alias{Method: "POST", Pattern: "/api/v1/workers/{worker_id}/sessions", Op: ops.OpSessionCreate}); err != nil {
		t.Fatal(err)
	}
	wid, other := ops.NewUUIDv7(), ops.NewUUIDv7()
	for _, c := range []struct{ method, body string }{
		{"GET", ""},
		// The path wins over a worker_id of the body.
		{"POST", `{"request_id":"` + ops.NewUUIDv7() + `","worker_id":"` + other + `"}`},
	} {
		got = nil
		before := r.decide.count()
		w := r.do(c.method, "/api/v1/workers/"+wid+"/sessions", c.body)
		if w.Code != 200 || got == nil || got.Resource != (authz.WorkerResource{ID: wid}) {
			t.Fatalf("%s: %d %s call=%+v", c.method, w.Code, w.Body, got)
		}
		if res := r.decide.calls[before].res; res != (authz.WorkerResource{ID: wid}) {
			t.Errorf("%s: decided on %#v", c.method, res)
		}
	}
	// The path is still validated and authorised like the canonical route.
	if w := r.do("GET", "/api/v1/workers/not-a-uuid/sessions", ""); errCode(t, w) != proto.CodeInvalidArgument {
		t.Errorf("bad worker_id: %d %s", w.Code, w.Body)
	}
	// An operation that names its Worker itself needs an explicit mapping.
	err := r.router.AddAlias(Alias{Method: "GET", Pattern: "/api/v1/ws/{worker_id}/screen", Op: ops.OpSessionSnapshot})
	if err == nil || !strings.Contains(err.Error(), "not an argument") {
		t.Errorf("worker_id on an op with a Target: err = %v", err)
	}
	// And a Hub operation has no Worker to route.
	err = r.router.AddAlias(Alias{Method: "GET", Pattern: "/api/v1/h/{worker_id}", Op: opPing})
	if err == nil || !strings.Contains(err.Error(), "not an argument") {
		t.Errorf("worker_id on a Hub op: err = %v", err)
	}
}

func TestMuxRedirects(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/api/v1", true},
		{"POST", "/api/v1/", false},
		{"POST", "/api/v1/ops/ping_read", false},
		{"POST", "/api/v1//x", true},
		{"POST", "/api/v1/./x", true},
		{"POST", "/api/v1/a/../x", true},
		{"POST", "/api/v1/x/", false},
		{"POST", "/healthz", false},
		{"CONNECT", "/api/v1//x", false},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://x/", nil)
		req.Method = c.method
		req.URL.Path, req.URL.RawPath = c.path, ""
		if got := muxRedirects(req); got != c.want {
			t.Errorf("%s %s: muxRedirects = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
