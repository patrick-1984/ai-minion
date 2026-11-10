package api

import (
	"context"
	"encoding/json"
	"errors"
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

// removeLookups answers authz from fixed tables: users with roles and Workers
// with owners; removing a Worker deletes its owner, like the real table.
type removeLookups struct {
	mu      sync.Mutex
	roles   map[string]authz.Role
	workers map[string]string
}

func (l *removeLookups) User(_ context.Context, id string) (authz.UserState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.roles[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return authz.UserState{Role: r, Active: true}, nil
}

func (l *removeLookups) WorkerOwner(_ context.Context, id string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.workers[id]
	if !ok {
		return "", authz.ErrNotFound
	}
	return o, nil
}

func (l *removeLookups) drop(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.workers, id)
}

type fakeRemover struct {
	look  *removeLookups
	mu    sync.Mutex
	calls []struct {
		actor workers.RemoveActor
		id    string
	}
	res workers.RemoveResult
	err error
}

func (f *fakeRemover) Remove(_ context.Context, a workers.RemoveActor, id string) (workers.RemoveResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, struct {
		actor workers.RemoveActor
		id    string
	}{a, id})
	f.mu.Unlock()
	if f.err != nil {
		return workers.RemoveResult{}, f.err
	}
	f.look.drop(id)
	return f.res, nil
}

// removeRig is the real dispatcher and authorizer with the real worker_remove
// declaration and a fake remover.
type removeRig struct {
	look  *removeLookups
	rem   *fakeRemover
	authn *fakeAuthn
	audit *fakeAudit
	mux   *http.ServeMux
	wid   string
}

func newRemoveRig(t *testing.T) *removeRig {
	t.Helper()
	op, ok := ops.Lookup(ops.OpWorkerRemove)
	if !ok {
		t.Fatal("worker_remove is not in the catalog")
	}
	cat := ops.NewRegistry()
	cat.Register(op)
	wid := ops.NewUUIDv7()
	look := &removeLookups{
		roles:   map[string]authz.Role{"owner": authz.RoleUser, "other": authz.RoleUser, "admin": authz.RoleAdmin},
		workers: map[string]string{wid: "owner"},
	}
	rem := &fakeRemover{look: look, res: workers.RemoveResult{Online: true, Revoked: true}}
	reg := NewRegistry(cat)
	if err := RegisterWorkerRemove(reg, &WorkerRemoveHandler{Remover: rem}); err != nil {
		t.Fatal(err)
	}
	r := &removeRig{look: look, rem: rem, authn: &fakeAuthn{}, audit: &fakeAudit{}, mux: http.NewServeMux(), wid: wid}
	d := &Dispatcher{Catalog: cat, Handlers: reg, Auth: r.authn, Authz: authz.New(look, look), Audit: r.audit, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, r.mux); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *removeRig) as(user string) {
	r.authn.id = Identity{Principal: authz.NewSessionPrincipal(user), Actor: user, Via: ViaSession}
}

func (r *removeRig) remove(id string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"request_id": ops.NewUUIDv7(), "worker_id": id})
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/ops/worker_remove", strings.NewReader(string(body))))
	return w
}

func TestWorkerRemoveOwnerAndAdminAreAllowedAndAnotherUserIsDenied(t *testing.T) {
	// A user who does not own the Worker learns nothing about it.
	r := newRemoveRig(t)
	r.as("other")
	if w := r.remove(r.wid); w.Code != 404 || errCode(t, w) != proto.CodeNotFound {
		t.Fatalf("foreign owner: %d %s", w.Code, w.Body)
	}
	if len(r.rem.calls) != 0 {
		t.Fatal("the removal ran for a foreign user")
	}

	r = newRemoveRig(t)
	r.as("owner")
	w := r.remove(r.wid)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"delivered"`) {
		t.Fatalf("owner: %d %s", w.Code, w.Body)
	}
	if len(r.rem.calls) != 1 || r.rem.calls[0].id != r.wid || r.rem.calls[0].actor.ID != "owner" {
		t.Errorf("calls = %+v", r.rem.calls)
	}

	r = newRemoveRig(t)
	r.as("admin")
	if w := r.remove(r.wid); w.Code != 200 {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
	if r.rem.calls[0].actor.ID != "admin" {
		t.Errorf("actor = %+v", r.rem.calls[0].actor)
	}
}

func TestWorkerRemoveActorCarriesActingAs(t *testing.T) {
	r := newRemoveRig(t)
	r.authn.id = Identity{Principal: authz.NewSessionPrincipal("owner"), Actor: "admin", ActingAs: "owner", Via: ViaSession}
	if w := r.remove(r.wid); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if a := r.rem.calls[0].actor; a.ID != "admin" || a.ActingAs != "owner" {
		t.Errorf("actor = %+v", a)
	}
}

func TestWorkerRemoveReportsWhenTheWorkerLearnsLater(t *testing.T) {
	for name, tc := range map[string]struct {
		res  workers.RemoveResult
		warn string
	}{
		"offline":     {workers.RemoveResult{}, ops.WarnRevokeOnNextConnect},
		"unconfirmed": {workers.RemoveResult{Online: true}, ops.WarnRevokeNotConfirmed},
		"confirmed":   {workers.RemoveResult{Online: true, Revoked: true}, ""},
	} {
		r := newRemoveRig(t)
		r.rem.res = tc.res
		r.as("owner")
		w := r.remove(r.wid)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"delivered"`) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		if got := strings.Contains(w.Body.String(), tc.warn); tc.warn != "" && !got {
			t.Errorf("%s: no warning %s in %s", name, tc.warn, w.Body)
		}
		if tc.warn == "" && !strings.Contains(w.Body.String(), `"warnings":[]`) {
			t.Errorf("%s: warnings in %s", name, w.Body)
		}
	}
}

func TestWorkerRemoveSecondCallAndErrorsAreEnveloped(t *testing.T) {
	r := newRemoveRig(t)
	r.as("owner")
	if w := r.remove(r.wid); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := r.remove(r.wid); errCode(t, w) != proto.CodeNotFound { // the Worker is gone: no owner, no access
		t.Errorf("second removal: %d %s", w.Code, w.Body)
	}
	r = newRemoveRig(t)
	r.as("owner")
	r.rem.err = errors.New("db exploded: /var/lib/minion.db")
	w := r.remove(r.wid)
	if errCode(t, w) != proto.CodeInternal || strings.Contains(w.Body.String(), "minion.db") {
		t.Errorf("internal failure: %d %s", w.Code, w.Body)
	}
	if w := r.remove("not-a-uuid"); errCode(t, w) != proto.CodeInvalidArgument {
		t.Errorf("bad worker id: %d %s", w.Code, w.Body)
	}
}

func TestRegisterWorkerRemoveChecksTheCatalog(t *testing.T) {
	reg := NewRegistry(ops.NewRegistry()) // no worker_remove declared
	if err := RegisterWorkerRemove(reg, &WorkerRemoveHandler{}); err == nil {
		t.Error("registered an operation that is not in the catalog")
	}
	h := &WorkerRemoveHandler{}
	if _, err := h.Remove(context.Background(), &Call{}, ops.WorkerRemoveArgs{}); err == nil {
		t.Error("a handler without a remover answered")
	}
}
