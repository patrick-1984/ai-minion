package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type fakeApprover struct {
	mu    sync.Mutex
	calls []struct{ actor, id, fp string }
	err   error
}

func (f *fakeApprover) Approve(_ context.Context, a workers.Actor, id, fp string) (workers.ApproveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ actor, id, fp string }{a.ID, id, fp})
	return workers.ApproveResult{Changed: true}, f.err
}

type approveRig struct {
	ap    *fakeApprover
	authn *fakeAuthn
	mux   *http.ServeMux
	wid   string
}

func newApproveRig(t *testing.T) *approveRig {
	t.Helper()
	op, ok := ops.Lookup(ops.OpWorkerApprove)
	if !ok {
		t.Fatal("worker_approve is not in the catalog")
	}
	cat := ops.NewRegistry()
	cat.Register(op)
	wid := ops.NewUUIDv7()
	look := &removeLookups{
		roles:   map[string]authz.Role{"owner": authz.RoleUser, "other": authz.RoleUser, "admin": authz.RoleAdmin},
		workers: map[string]string{wid: "owner"},
	}
	ap := &fakeApprover{}
	reg := NewRegistry(cat)
	if err := RegisterWorkerApprove(reg, ap); err != nil {
		t.Fatal(err)
	}
	r := &approveRig{ap: ap, authn: &fakeAuthn{}, mux: http.NewServeMux(), wid: wid}
	d := &Dispatcher{Catalog: cat, Handlers: reg, Auth: r.authn, Authz: authz.New(look, look), Audit: &fakeAudit{}, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, r.mux); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *approveRig) approve(fp string) *httptest.ResponseRecorder {
	m := map[string]any{"request_id": ops.NewUUIDv7(), "worker_id": r.wid}
	if fp != "" {
		m["fingerprint"] = fp
	}
	body, _ := json.Marshal(m)
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/ops/worker_approve", strings.NewReader(string(body))))
	return w
}

// scopeManage grants manage and read on everything, like an admin's key.
type scopeManage struct{}

func (scopeManage) Actions(authz.ResolvedResource) []ops.Action {
	return []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}
}

// scopeRead grants read only.
type scopeRead struct{}

func (scopeRead) Actions(authz.ResolvedResource) []ops.Action { return []ops.Action{ops.ActionRead} }

func TestWorkerApproveOwnerAdminAndAdminKeyMayApproveAndOthersMayNot(t *testing.T) {
	fp := strings.Repeat("a", 52)
	session := func(u string) Identity {
		return Identity{Principal: authz.NewSessionPrincipal(u), Actor: u, Via: ViaSession}
	}
	key := func(owner string, sc authz.Scope, bound string) Identity {
		return Identity{Principal: authz.NewAPIKeyPrincipal(owner, sc, bound), Actor: "key:k", Via: ViaAPIKey}
	}
	for name, tc := range map[string]struct {
		id   func(wid string) Identity
		code int // 0: any refusal
	}{
		"owner":                 {func(string) Identity { return session("owner") }, 200},
		"admin":                 {func(string) Identity { return session("admin") }, 200},
		"admin key with manage": {func(string) Identity { return key("admin", scopeManage{}, "") }, 200},
		"foreign user":          {func(string) Identity { return session("other") }, 404},
		"read-only key":         {func(string) Identity { return key("owner", scopeRead{}, "") }, 403},
		"the pending Worker itself": {func(wid string) Identity {
			return Identity{Principal: authz.NewWorkerPrincipal(wid), Actor: "worker:" + wid, Via: ViaAPIKey}
		}, 0},
		"a key bound to it":      {func(wid string) Identity { return key("owner", scopeManage{}, wid) }, 403},
		"a key bound to another": {func(string) Identity { return key("owner", scopeManage{}, ops.NewUUIDv7()) }, 200},
	} {
		t.Run(name, func(t *testing.T) {
			r := newApproveRig(t)
			r.authn.id = tc.id(r.wid)
			w := r.approve(fp)
			switch {
			case tc.code == 0:
				if w.Code == 200 || len(r.ap.calls) != 0 {
					t.Fatalf("a Worker identity approved: %d %s", w.Code, w.Body)
				}
			case w.Code != tc.code:
				t.Fatalf("%d %s, want %d", w.Code, w.Body, tc.code)
			case tc.code == 200:
				if len(r.ap.calls) != 1 || r.ap.calls[0].id != r.wid || r.ap.calls[0].fp != fp {
					t.Errorf("calls = %+v", r.ap.calls)
				}
			case len(r.ap.calls) != 0:
				t.Errorf("the approval ran")
			}
		})
	}
}

func TestWorkerApproveRejectsAMalformedFingerprintAndPassesServiceErrors(t *testing.T) {
	r := newApproveRig(t)
	r.authn.id = Identity{Principal: authz.NewSessionPrincipal("owner"), Actor: "owner", Via: ViaSession}
	if w := r.approve("short"); w.Code != 400 {
		t.Errorf("malformed fingerprint: %d %s", w.Code, w.Body)
	}
	r.ap.err = proto.NewError(proto.CodeConflict, "the fingerprint does not match")
	if w := r.approve(strings.Repeat("b", 52)); w.Code != 409 || errCode(t, w) != proto.CodeConflict {
		t.Errorf("service conflict: %d %s", w.Code, w.Body)
	}
}
