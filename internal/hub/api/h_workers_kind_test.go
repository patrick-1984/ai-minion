package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// kindCatalog holds the Worker administration and trust operations.
func kindCatalog(t testing.TB) *ops.Registry {
	t.Helper()
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpWorkerReassign, ops.OpWorkerRestrict, ops.OpWorkerRename, ops.OpTrustSet, ops.OpTrustList} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	return cat
}

// lookups is authz over a fixed user table and the real workers table.
type lookups struct {
	users map[string]authz.UserState
	st    *store.Store
}

func (l lookups) User(_ context.Context, id string) (authz.UserState, error) {
	u, ok := l.users[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return u, nil
}

func (l lookups) WorkerOwner(ctx context.Context, id string) (string, error) {
	o, err := l.st.WorkerOwner(ctx, id)
	if err != nil {
		return "", authz.ErrNotFound
	}
	return o, nil
}

// hookDecider delegates to the real authorizer and, after the n-th decision,
// runs after(n): the point where a test lets a reassignment land between two
// steps of a request.
type hookDecider struct {
	inner Decider
	mu    sync.Mutex
	n     int
	after func(n int)
}

func (h *hookDecider) Decide(ctx context.Context, p authz.Principal, a ops.ActionSet, res authz.Resource) error {
	err := h.inner.Decide(ctx, p, a, res)
	h.mu.Lock()
	h.n++
	n, f := h.n, h.after
	h.mu.Unlock()
	if f != nil {
		f(n)
	}
	return err
}

// kindStack is the real services over a real store, authz and dispatcher; only
// authentication is faked, so each test picks the caller.
type kindStack struct {
	t             *testing.T
	st            *store.Store
	mux           *http.ServeMux
	authn         *fakeAuthn
	audit         *fakeAudit
	reg           *workers.Registry
	az            *hookDecider
	admin, u1, u2 string
}

func newKindStack(t *testing.T) *kindStack {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	k := &kindStack{t: t, st: st, authn: &fakeAuthn{}, audit: &fakeAudit{}, reg: workers.NewRegistry(), mux: http.NewServeMux()}
	users := map[string]authz.UserState{}
	add := func(name, role string) string {
		u := store.UserRow{ID: store.NewID(), Username: name, PasswordHash: "x", CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1}
		if err := st.CreateUser(context.Background(), store.CreateUserParams{User: u, Role: role}); err != nil {
			t.Fatal(err)
		}
		users[u.ID] = authz.UserState{Role: authz.Role(role), Active: true}
		return u.ID
	}
	k.admin, k.u1, k.u2 = add("root", "admin"), add("alice", "user"), add("bob", "user")

	cat := kindCatalog(t)
	handlers := NewRegistry(cat)
	re, err := workers.NewReassigner(workers.ReassignConfig{Store: st, Connections: k.reg})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := workers.NewRestrictions(workers.RestrictionsConfig{Store: st, Connections: k.reg})
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterWorkerKindHandlers(handlers, re, rs); err != nil {
		t.Fatal(err)
	}
	k.az = &hookDecider{inner: authz.New(lookups{users, st}, lookups{users, st})}
	if err := RegisterTrustHandlers(handlers, authz.NewTrust(TrustStore(st), nil), k.az); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{
		Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az,
		Audit: k.audit, Log: testLogger(&strings.Builder{}),
	}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *kindStack) worker(owner, name, kind string) store.WorkerRow {
	k.t.Helper()
	pub := make([]byte, 32)
	if _, err := rand.Read(pub); err != nil {
		k.t.Fatal(err)
	}
	w := store.WorkerRow{ID: store.NewID(), Pub: pub, Name: name, OwnerID: owner, Kind: kind, CreatedAt: 1}
	if err := k.st.InsertWorker(context.Background(), nil, w); err != nil {
		k.t.Fatal(err)
	}
	return w
}

// as makes the next calls come from a browser session of the user.
func (k *kindStack) as(userID string) {
	k.authn.mu.Lock()
	defer k.authn.mu.Unlock()
	k.authn.id = Identity{
		Principal: authz.NewSessionPrincipal(userID), Actor: userID, Via: ViaSession, IP: netip.MustParseAddr("192.0.2.7"),
	}
}

func (k *kindStack) call(op string, args map[string]any) (int, string) {
	k.t.Helper()
	if _, ok := args["request_id"]; !ok && opNeedsRequestID(op) {
		args["request_id"] = ops.NewUUIDv7()
	}
	b, _ := json.Marshal(args)
	rec := newRecorder()
	k.mux.ServeHTTP(rec, newJSONRequest(op, string(b)))
	return rec.Code, rec.Body.String()
}

func opNeedsRequestID(op string) bool { return op != ops.OpTrustList }

func kindErrCode(t *testing.T, body string) proto.Code {
	t.Helper()
	var e struct {
		Error struct {
			Code proto.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not an error body: %s", body)
	}
	return e.Error.Code
}

func TestWorkerReassignNeedsAnAdminAndAnInfrastructureWorker(t *testing.T) {
	k := newKindStack(t)
	infra := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
	private := k.worker(k.u1, "mine", store.WorkerKindPrivate)
	args := func(w store.WorkerRow, to string) map[string]any {
		return map[string]any{"worker_id": w.ID, "user_id": to}
	}

	// The owner holds operate but not manage: forbidden, not hidden.
	k.as(k.u1)
	if code, body := k.call(ops.OpWorkerReassign, args(infra, k.u2)); code != 403 || kindErrCode(t, body) != proto.CodeForbidden {
		t.Errorf("owner: %d %s", code, body)
	}
	// Somebody else cannot even see the Worker.
	k.as(k.u2)
	if code, body := k.call(ops.OpWorkerReassign, args(infra, k.u2)); code != 404 || kindErrCode(t, body) != proto.CodeNotFound {
		t.Errorf("stranger: %d %s", code, body)
	}
	// An admin cannot reassign a private Worker either.
	k.as(k.admin)
	if code, body := k.call(ops.OpWorkerReassign, args(private, k.u2)); code != 403 || kindErrCode(t, body) != proto.CodeForbidden {
		t.Errorf("admin on a private Worker: %d %s", code, body)
	}
	if o, _ := k.st.WorkerOwner(context.Background(), infra.ID); o != k.u1 {
		t.Fatalf("owner = %s after the refusals", o)
	}
	// An admin reassigns an infrastructure Worker.
	if code, body := k.call(ops.OpWorkerReassign, args(infra, k.u2)); code != 200 || !strings.Contains(body, `"outcome":"delivered"`) {
		t.Fatalf("admin on an infrastructure Worker: %d %s", code, body)
	}
	if o, _ := k.st.WorkerOwner(context.Background(), infra.ID); o != k.u2 {
		t.Errorf("owner = %s", o)
	}
	// The dispatcher audits the write under the op name.
	var ok bool
	for _, ev := range k.audit.all() {
		ok = ok || (ev.Action == ops.OpWorkerReassign && ev.Target == infra.ID && ev.Outcome == AuditOutcomeSuccess)
	}
	if !ok {
		t.Errorf("audit = %+v", k.audit.all())
	}
}

func TestWorkerReassignNameCollisionIsConflict(t *testing.T) {
	k := newKindStack(t)
	infra := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
	k.worker(k.u2, "pi", store.WorkerKindPrivate)
	k.as(k.admin)
	code, body := k.call(ops.OpWorkerReassign, map[string]any{"worker_id": infra.ID, "user_id": k.u2})
	if code != 409 || kindErrCode(t, body) != proto.CodeConflict {
		t.Errorf("%d %s", code, body)
	}
}

func TestWorkerRestrictByOwnerAndAdminButNotByAStranger(t *testing.T) {
	k := newKindStack(t)
	w := k.worker(k.u1, "pi", store.WorkerKindPrivate)
	args := func(on bool) map[string]any { return map[string]any{"worker_id": w.ID, "preview_only": on} }
	preview := func() bool { on, _ := k.st.WorkerPreviewOnly(context.Background(), w.ID); return on }

	k.as(k.u2)
	if code, body := k.call(ops.OpWorkerRestrict, args(true)); code != 404 {
		t.Errorf("stranger: %d %s", code, body)
	}
	if preview() {
		t.Fatal("a stranger restricted the Worker")
	}
	k.as(k.u1)
	if code, body := k.call(ops.OpWorkerRestrict, args(true)); code != 200 {
		t.Fatalf("owner: %d %s", code, body)
	}
	if !preview() {
		t.Error("the owner's restriction was not stored")
	}
	k.as(k.admin)
	if code, body := k.call(ops.OpWorkerRestrict, args(false)); code != 200 {
		t.Fatalf("admin: %d %s", code, body)
	}
	if preview() {
		t.Error("the admin's change was not stored")
	}
}

func TestWorkerRenameConflictAndOwnership(t *testing.T) {
	k := newKindStack(t)
	a := k.worker(k.u1, "pi", store.WorkerKindPrivate)
	k.worker(k.u1, "nuc", store.WorkerKindPrivate)
	k.as(k.u2)
	if code, _ := k.call(ops.OpWorkerRename, map[string]any{"worker_id": a.ID, "name": "x"}); code != 404 {
		t.Errorf("stranger: %d", code)
	}
	k.as(k.u1)
	if code, body := k.call(ops.OpWorkerRename, map[string]any{"worker_id": a.ID, "name": "nuc"}); code != 409 {
		t.Errorf("collision: %d %s", code, body)
	}
	if code, body := k.call(ops.OpWorkerRename, map[string]any{"worker_id": a.ID, "name": "bad name"}); code != 400 {
		t.Errorf("invalid name: %d %s", code, body)
	}
	if code, body := k.call(ops.OpWorkerRename, map[string]any{"worker_id": a.ID, "name": "lab"}); code != 200 {
		t.Fatalf("rename: %d %s", code, body)
	}
	if got, _ := k.st.WorkerByID(context.Background(), a.ID); got.Name != "lab" {
		t.Errorf("name = %q", got.Name)
	}
}

func TestTrustSetAndListThroughTheDispatcher(t *testing.T) {
	k := newKindStack(t)
	w := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
	k.as(k.u1)
	for _, args := range []map[string]any{
		{"worker_id": w.ID},
		{"worker_id": w.ID, "session_name": "minion-1"},
	} {
		if code, body := k.call(ops.OpTrustSet, args); code != 200 {
			t.Fatalf("trust_set %v: %d %s", args, code, body)
		}
	}
	code, body := k.call(ops.OpTrustList, map[string]any{})
	if code != 200 {
		t.Fatalf("trust_list: %d %s", code, body)
	}
	var list ops.TrustListResult
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list.Trust) != 2 ||
		list.Trust[0].WorkerID != w.ID || list.Trust[0].SessionName != "" || list.Trust[1].SessionName != "minion-1" {
		t.Fatalf("list = %s (%v)", body, err)
	}
	// Remove the whole-Worker mark only.
	if code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID, "remove": true}); code != 200 {
		t.Fatalf("remove: %d %s", code, body)
	}
	if _, body := k.call(ops.OpTrustList, map[string]any{}); !strings.Contains(body, "minion-1") || strings.Count(body, "worker_id") != 1 {
		t.Errorf("list after remove = %s", body)
	}
	// Another user's list is their own.
	k.as(k.u2)
	if _, body := k.call(ops.OpTrustList, map[string]any{}); !strings.Contains(body, `"trust":[]`) {
		t.Errorf("another user's list = %s", body)
	}
}

func TestTrustSetNeedsReadOnTheWorker(t *testing.T) {
	k := newKindStack(t)
	w := k.worker(k.u1, "pi", store.WorkerKindPrivate)
	k.as(k.u2)
	if code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID}); code != 404 {
		t.Errorf("stranger: %d %s", code, body)
	}
	if rows, _ := k.st.ListTrust(context.Background(), k.u2); len(rows) != 0 {
		t.Errorf("a stranger stored trust: %+v", rows)
	}
	// An admin may read every Worker and so may trust it; the mark is theirs.
	k.as(k.admin)
	if code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID}); code != 200 {
		t.Fatalf("admin: %d %s", code, body)
	}
	if rows, _ := k.st.ListTrust(context.Background(), k.admin); len(rows) != 1 {
		t.Errorf("admin rows = %+v", rows)
	}
	if rows, _ := k.st.ListTrust(context.Background(), k.u1); len(rows) != 0 {
		t.Errorf("the owner inherited the admin's trust: %+v", rows)
	}
}

// reassignDirect moves the Worker as the reassignment service does, without
// the dispatcher, so that a test can run it in the middle of another request.
func (k *kindStack) reassignDirect(w store.WorkerRow, to string) {
	k.t.Helper()
	if _, err := k.st.ReassignWorker(context.Background(), store.ReassignParams{WorkerID: w.ID, NewOwnerID: to, AtMS: 1}); err != nil {
		k.t.Fatalf("reassign: %v", err)
	}
}

// A trust_set that was authorized under the previous owner must not bring a
// mark back after the reassignment deleted all trust, wherever the
// reassignment lands in the request.
func TestTrustSetCannotOutliveAReassignmentThatLandsMidRequest(t *testing.T) {
	// Case 1: between the dispatcher's decision and the handler. The handler
	// decides again and the old owner no longer sees the Worker.
	t.Run("after the dispatcher decision", func(t *testing.T) {
		k := newKindStack(t)
		w := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
		k.as(k.u1)
		k.az.after = func(n int) {
			if n == 1 {
				k.reassignDirect(w, k.u2)
			}
		}
		code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID})
		if code != 404 {
			t.Errorf("trust_set = %d %s, want 404", code, body)
		}
		for _, u := range []string{k.u1, k.u2} {
			if rows, _ := k.st.ListTrust(context.Background(), u); len(rows) != 0 {
				t.Errorf("trust of %s after the reassignment: %+v", u, rows)
			}
		}
	})
	// Case 2: after the handler's second decision, right before the insert.
	// The insert is conditional on the owner it decided under.
	t.Run("before the insert", func(t *testing.T) {
		k := newKindStack(t)
		w := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
		k.as(k.u1)
		k.az.after = func(n int) {
			if n == 2 {
				k.reassignDirect(w, k.u2)
			}
		}
		code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID})
		if code != 409 || kindErrCode(t, body) != proto.CodeConflict {
			t.Errorf("trust_set = %d %s, want 409 conflict", code, body)
		}
		for _, u := range []string{k.u1, k.u2} {
			if rows, _ := k.st.ListTrust(context.Background(), u); len(rows) != 0 {
				t.Errorf("trust of %s after the reassignment: %+v", u, rows)
			}
		}
	})
	// An admin's mark that was decided under the old owner is refused the
	// same way.
	t.Run("admin before the insert", func(t *testing.T) {
		k := newKindStack(t)
		w := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
		k.as(k.admin)
		k.az.after = func(n int) {
			if n == 2 {
				k.reassignDirect(w, k.u2)
			}
		}
		if code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID}); code != 409 {
			t.Errorf("trust_set = %d %s, want 409", code, body)
		}
		if rows, _ := k.st.ListTrust(context.Background(), k.admin); len(rows) != 0 {
			t.Errorf("the admin's mark survived: %+v", rows)
		}
	})
}

// The reassignment lands after the dispatcher's authorization but before the
// handler runs, which is the window in which the handler must not capture the
// new owner as the one the request was decided under. An admin passes every
// decision, so only the owner captured before the first decision stops it.
func TestTrustSetOfAnAdminSpanningAReassignmentBeforeTheHandlerStoresNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(k *kindStack, w store.WorkerRow) func(n int)
	}{
		{"after the dispatcher decision", func(k *kindStack, w store.WorkerRow) func(int) {
			return func(n int) {
				if n == 1 {
					k.reassignDirect(w, k.u2)
				}
			}
		}},
		{"after the handler decision", func(k *kindStack, w store.WorkerRow) func(int) {
			return func(n int) {
				if n == 2 {
					k.reassignDirect(w, k.u2)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKindStack(t)
			w := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
			k.as(k.admin)
			k.az.after = tc.hook(k, w)
			code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID})
			if code != 409 || kindErrCode(t, body) != proto.CodeConflict {
				t.Errorf("trust_set = %d %s, want 409 conflict", code, body)
			}
			for _, u := range []string{k.admin, k.u1, k.u2} {
				if rows, _ := k.st.ListTrust(context.Background(), u); len(rows) != 0 {
					t.Errorf("trust of %s after the reassignment: %+v", u, rows)
				}
			}
		})
	}
}

func TestTrustDuringActAsBelongsToTheActor(t *testing.T) {
	k := newKindStack(t)
	w := k.worker(k.u1, "pi", store.WorkerKindPrivate)
	k.authn.mu.Lock()
	k.authn.id = Identity{
		Principal: authz.NewActAsPrincipal(k.admin, k.u1, timeInFuture()), Actor: k.admin, ActingAs: k.u1,
		Via: ViaSession, IP: netip.MustParseAddr("192.0.2.7"),
	}
	k.authn.mu.Unlock()
	if code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if rows, _ := k.st.ListTrust(context.Background(), k.admin); len(rows) != 1 {
		t.Errorf("the actor's rows = %+v", rows)
	}
	if rows, _ := k.st.ListTrust(context.Background(), k.u1); len(rows) != 0 {
		t.Errorf("trust was stored for the user acted as: %+v", rows)
	}
	var ev *audit.Event
	for _, e := range k.audit.all() {
		if e.Action == ops.OpTrustSet {
			ev = &e
		}
	}
	if ev == nil || ev.Actor != k.admin || ev.ActingAs != k.u1 {
		t.Errorf("audit = %+v", ev)
	}
}

func TestWorkerIdentityHasNoTrust(t *testing.T) {
	k := newKindStack(t)
	w := k.worker(k.u1, "pi", store.WorkerKindPrivate)
	k.authn.mu.Lock()
	k.authn.id = Identity{Principal: authz.NewWorkerPrincipal(w.ID), Actor: "worker:" + w.ID, Via: ViaAPIKey}
	k.authn.mu.Unlock()
	if code, body := k.call(ops.OpTrustSet, map[string]any{"worker_id": w.ID}); code != 403 {
		t.Errorf("%d %s", code, body)
	}
}

// fakeKinds records calls to check how the handlers hand over the caller.
type fakeKinds struct {
	mu     sync.Mutex
	actors []workers.Actor
}

func (f *fakeKinds) Reassign(_ context.Context, a workers.Actor, _, _ string) (workers.ReassignResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, a)
	return workers.ReassignResult{}, nil
}

func (f *fakeKinds) Rename(_ context.Context, a workers.Actor, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, a)
	return nil
}

func (f *fakeKinds) Restrict(_ context.Context, a workers.Actor, _ string, _ bool) (workers.RestrictResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors = append(f.actors, a)
	return workers.RestrictResult{}, nil
}

func TestHandlersPassTheAuditIdentity(t *testing.T) {
	cat := kindCatalog(t)
	reg := NewRegistry(cat)
	f := &fakeKinds{}
	if err := RegisterWorkerKindHandlers(reg, f, f); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		authn: &fakeAuthn{id: testIdentity}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: reg, mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.authn.id.ActingAs = "user-9"
	r.d = &Dispatcher{Catalog: cat, Handlers: reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	id, rq := ops.NewUUIDv7(), ops.NewUUIDv7()
	for op, body := range map[string]string{
		ops.OpWorkerReassign: `{"request_id":"` + rq + `","worker_id":"` + id + `","user_id":"` + id + `"}`,
		ops.OpWorkerRename:   `{"request_id":"` + rq + `","worker_id":"` + id + `","name":"pi"}`,
		ops.OpWorkerRestrict: `{"request_id":"` + rq + `","worker_id":"` + id + `","preview_only":true}`,
	} {
		if w := r.do("POST", "/api/v1/ops/"+op, body); w.Code != 200 {
			t.Fatalf("%s: %d %s", op, w.Code, w.Body)
		}
	}
	if len(f.actors) != 3 {
		t.Fatalf("actors = %+v", f.actors)
	}
	for _, a := range f.actors {
		if a.ID != "user-1" || a.ActingAs != "user-9" {
			t.Errorf("actor = %+v", a)
		}
	}
}

func TestRegisterHandlersNeedTheirServices(t *testing.T) {
	cat := kindCatalog(t)
	if err := RegisterWorkerKindHandlers(NewRegistry(cat), nil, nil); err == nil {
		t.Error("RegisterWorkerKindHandlers accepted no services")
	}
	if err := RegisterTrustHandlers(NewRegistry(cat), nil, nil); err == nil {
		t.Error("RegisterTrustHandlers accepted no service")
	}
}

func timeInFuture() time.Time { return time.Now().Add(time.Hour) }

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func newJSONRequest(op, body string) *http.Request {
	req := httptest.NewRequest("POST", "/api/v1/ops/"+op, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// A trust_set that the dispatcher refuses after the owner was captured leaves
// no pin behind, and a handler reached without a pin fails closed.
func TestTrustOwnerPinsAreTakenOnceAndDroppedWithTheRequest(t *testing.T) {
	p := &ownerPins{m: map[weak.Pointer[ops.TrustSetArgs]]string{}}
	a := &ops.TrustSetArgs{}
	p.put(a, "alice")
	if o, ok := p.take(a); !ok || o != "alice" {
		t.Fatalf("take = %q, %v", o, ok)
	}
	if _, ok := p.take(a); ok {
		t.Error("a pin was taken twice")
	}
	func() {
		p.put(&ops.TrustSetArgs{}, "bob") // a refused request: nobody takes it
	}()
	for i := 0; i < 50; i++ {
		runtime.GC()
		p.mu.Lock()
		n := len(p.m)
		p.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the pin of a request that never reached the handler was not dropped")
}
