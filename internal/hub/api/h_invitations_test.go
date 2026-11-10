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

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// invCatalog is a catalog with just the three invitation operations.
func invCatalog(t testing.TB) *ops.Registry {
	t.Helper()
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpInvitationCreate, ops.OpInvitationList, ops.OpInvitationRevoke} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	return cat
}

// fakeInvitations records the callers and answers from fixed values.
type fakeInvitations struct {
	mu      sync.Mutex
	callers []workers.InvitationCaller
	create  ops.InvitationCreateArgs
	all     bool
	revoked string
	err     error
}

func (f *fakeInvitations) Create(_ context.Context, c workers.InvitationCaller, a ops.InvitationCreateArgs) (ops.InvitationCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers, f.create = append(f.callers, c), a
	return ops.InvitationCreateResult{ID: "inv-1", Key: "mnk_fake.secret", Mode: a.Mode, TTL: a.TTL}, f.err
}

func (f *fakeInvitations) List(_ context.Context, c workers.InvitationCaller, all bool) (ops.InvitationListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers, f.all = append(f.callers, c), all
	return ops.InvitationListResult{}, f.err
}

func (f *fakeInvitations) Revoke(_ context.Context, c workers.InvitationCaller, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers, f.revoked = append(f.callers, c), id
	return f.err
}

type invRig struct {
	*rig
	svc *fakeInvitations
}

func newInvRig(t *testing.T) *invRig {
	t.Helper()
	cat := invCatalog(t)
	r := &rig{
		authn: &fakeAuthn{id: testIdentity}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	svc := &fakeInvitations{}
	if err := RegisterInvitationHandlers(r.reg, svc, r.decide); err != nil {
		t.Fatal(err)
	}
	return &invRig{rig: r, svc: svc}
}

// adminOnManage makes the decider grant manage and refuse everything else
// that is not the caller's own operate/read: it models an admin.
func adminDecider(c decideCall) error {
	return nil
}

func userDecider(c decideCall) error {
	if c.accepted.Permits(ops.ActionManage) && len(c.accepted) == 1 {
		return proto.NewError(proto.CodeForbidden, "not allowed")
	}
	return nil
}

func TestInvitationCreateHandlerPassesTheCallerAndArguments(t *testing.T) {
	r := newInvRig(t)
	r.decide.fn = userDecider
	w := r.do("POST", "/api/v1/ops/invitation_create", `{"mode":"reusable","ttl":"1w","name":"lab","kind":"private"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var res ops.InvitationCreateResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Key != "mnk_fake.secret" || res.ID != "inv-1" {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if len(r.svc.callers) != 1 {
		t.Fatalf("callers = %+v", r.svc.callers)
	}
	c := r.svc.callers[0]
	if c.UserID != "user-1" || c.Actor != "user-1" || c.ActingAs != "" || c.Admin {
		t.Errorf("caller = %+v", c)
	}
	if a := r.svc.create; a.Mode != "reusable" || a.TTL != "1w" || a.Name != "lab" || a.Kind != "private" {
		t.Errorf("args = %+v", a)
	}
	// The dispatcher decided operate on the caller's own user resource; the
	// handler asked for manage on the same resource to tell admins apart.
	calls := r.decide.calls
	if len(calls) != 2 {
		t.Fatalf("decide calls = %d", len(calls))
	}
	if calls[0].accepted.String() != "operate" || calls[0].res != (authz.UserResource{ID: "user-1"}) {
		t.Errorf("first decision = %+v", calls[0])
	}
	if calls[1].accepted.String() != "manage" || calls[1].res != (authz.UserResource{ID: "user-1"}) {
		t.Errorf("admin probe = %+v", calls[1])
	}
}

func TestInvitationHandlersSeeAnAdmin(t *testing.T) {
	r := newInvRig(t)
	r.decide.fn = adminDecider
	r.authn.id = Identity{Principal: authz.NewSessionPrincipal("root"), Actor: "root", Via: ViaSession, IP: testIdentity.IP}
	if w := r.do("POST", "/api/v1/ops/invitation_list", `{"all":true}`); w.Code != 200 || !r.svc.all {
		t.Fatalf("list: %d %s all=%v", w.Code, w.Body, r.svc.all)
	}
	if !r.svc.callers[0].Admin {
		t.Errorf("caller = %+v, want Admin", r.svc.callers[0])
	}
	if raw := r.do("GET", "/api/v1/ops/invitation_list", "").Body.String(); !strings.Contains(raw, `"invitations":[]`) {
		t.Errorf("empty list = %s", raw)
	}
}

func TestInvitationHandlerActAsKeepsBothIdentities(t *testing.T) {
	r := newInvRig(t)
	r.decide.fn = userDecider
	r.authn.id = Identity{
		Principal: authz.Principal{Kind: authz.KindActAs, UserID: "alice", ActorID: "root"}, Actor: "root", ActingAs: "alice",
		Via: ViaSession, IP: testIdentity.IP,
	}
	if w := r.do("POST", "/api/v1/ops/invitation_create", `{"mode":"one_time","ttl":"1h"}`); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	c := r.svc.callers[0]
	if c.UserID != "alice" || c.Actor != "root" || c.ActingAs != "alice" {
		t.Errorf("caller = %+v", c)
	}
}

func TestInvitationRevokeHandler(t *testing.T) {
	r := newInvRig(t)
	r.decide.fn = userDecider
	id := ops.NewUUIDv7()
	w := r.do("POST", "/api/v1/ops/invitation_revoke", `{"request_id":"`+ops.NewUUIDv7()+`","id":"`+id+`"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if r.svc.revoked != id {
		t.Errorf("revoked %q, want %q", r.svc.revoked, id)
	}
	// The dispatcher audits the write op.
	evs := r.audit.all()
	if len(evs) != 1 || evs[0].Action != ops.OpInvitationRevoke || evs[0].Outcome != AuditOutcomeSuccess {
		t.Errorf("audit = %+v", evs)
	}
	// Bad arguments never reach the service.
	r.svc.revoked = ""
	for name, body := range map[string]string{
		"no request id": `{"id":"` + id + `"}`,
		"bad id":        `{"request_id":"` + ops.NewUUIDv7() + `","id":"x"}`,
	} {
		if w := r.do("POST", "/api/v1/ops/invitation_revoke", body); w.Code != 400 || r.svc.revoked != "" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestInvitationServiceErrorsKeepTheirCodes(t *testing.T) {
	r := newInvRig(t)
	r.decide.fn = userDecider
	for err, want := range map[error]int{
		proto.NewError(proto.CodeForbidden, "only an admin"): 403,
		proto.NewError(proto.CodeNotFound, "gone"):           404,
		errors.New("database exploded"):                      500,
	} {
		r.svc.err = err
		w := r.do("POST", "/api/v1/ops/invitation_create", `{"mode":"one_time","ttl":"1h"}`)
		if w.Code != want {
			t.Errorf("%v: status %d, want %d", err, w.Code, want)
		}
		if strings.Contains(w.Body.String(), "exploded") {
			t.Errorf("the response leaks the internal error: %s", w.Body)
		}
	}
}

func TestInvitationAdminProbeFailureIsNotTreatedAsAdmin(t *testing.T) {
	r := newInvRig(t)
	r.decide.fn = func(c decideCall) error {
		if c.accepted.Permits(ops.ActionManage) {
			return errors.New("user lookup failed")
		}
		return nil
	}
	w := r.do("POST", "/api/v1/ops/invitation_create", `{"mode":"one_time","ttl":"1h","kind":"infrastructure"}`)
	if w.Code != 500 || len(r.svc.callers) != 0 {
		t.Errorf("status %d, service called %d times: %s", w.Code, len(r.svc.callers), w.Body)
	}
}

func TestInvitationValidationRunsBeforeTheHandler(t *testing.T) {
	r := newInvRig(t)
	for name, body := range map[string]string{
		"mode": `{"mode":"x","ttl":"1h"}`,
		"ttl":  `{"mode":"one_time","ttl":"3h"}`,
		"kind": `{"mode":"one_time","ttl":"1h","kind":"x"}`,
	} {
		if w := r.do("POST", "/api/v1/ops/invitation_create", body); w.Code != 400 {
			t.Errorf("%s: status %d", name, w.Code)
		}
	}
	if len(r.svc.callers) != 0 {
		t.Error("the service ran for an invalid request")
	}
}

func TestRegisterInvitationHandlersNeedsItsPorts(t *testing.T) {
	reg := NewRegistry(invCatalog(t))
	if err := RegisterInvitationHandlers(reg, nil, &fakeDecider{}); err == nil {
		t.Error("no service accepted")
	}
	if err := RegisterInvitationHandlers(reg, &fakeInvitations{}, nil); err == nil {
		t.Error("no decider accepted")
	}
	if err := RegisterInvitationHandlers(reg, &fakeInvitations{}, &fakeDecider{}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterInvitationHandlers(reg, &fakeInvitations{}, &fakeDecider{}); err == nil {
		t.Error("a second registration was accepted")
	}
	// A catalog without the operations fails at registration, not on a request.
	if err := RegisterInvitationHandlers(NewRegistry(ops.NewRegistry()), &fakeInvitations{}, &fakeDecider{}); err == nil {
		t.Error("registration against an empty catalog succeeded")
	}
}

// --- the whole path with the real service, store and authorizer ---

type fakeUsers map[string]authz.UserState

func (f fakeUsers) User(_ context.Context, id string) (authz.UserState, error) {
	u, ok := f[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return u, nil
}

type noWorkers struct{}

func (noWorkers) WorkerOwner(context.Context, string) (string, error) { return "", authz.ErrNotFound }

type realRig struct {
	*rig
	st  *store.Store
	log *audit.Log
}

func newRealRig(t *testing.T) *realRig {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	alog := audit.New(st)
	svc, err := workers.NewInvitations(workers.InvitationsConfig{Store: st, HubFingerprint: key.Fingerprint(), Auditor: alog})
	if err != nil {
		t.Fatal(err)
	}
	az := authz.New(fakeUsers{
		"alice": {Role: authz.RoleUser, Active: true},
		"bob":   {Role: authz.RoleUser, Active: true},
		"root":  {Role: authz.RoleAdmin, Active: true},
	}, noWorkers{})
	cat := invCatalog(t)
	r := &rig{authn: &fakeAuthn{id: testIdentity}, audit: &fakeAudit{}, reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{}}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: az, Audit: alog, Log: testLogger(r.logs)}
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	if err := RegisterInvitationHandlers(r.reg, svc, az); err != nil {
		t.Fatal(err)
	}
	return &realRig{rig: r, st: st, log: alog}
}

func (r *realRig) as(user string) {
	r.authn.id = Identity{Principal: authz.NewSessionPrincipal(user), Actor: user, Via: ViaSession, IP: testIdentity.IP}
}

func (r *realRig) call(t *testing.T, op, body string, into any) *httptest.ResponseRecorder {
	t.Helper()
	w := r.do("POST", "/api/v1/ops/"+op, body)
	if into != nil && w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			t.Fatalf("%s: %v: %s", op, err, w.Body)
		}
	}
	return w
}

func TestInvitationsEndToEndThroughTheDispatcher(t *testing.T) {
	r := newRealRig(t)
	var aliceKey, rootKey ops.InvitationCreateResult

	// A user creates a key in their own space.
	r.as("alice")
	if w := r.call(t, "invitation_create", `{"mode":"one_time","ttl":"4h","name":"pi"}`, &aliceKey); w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if !strings.HasPrefix(aliceKey.Key, "mnk_") || aliceKey.Kind != "private" || aliceKey.ExpiresAt == nil {
		t.Errorf("created = %+v", aliceKey)
	}
	// ... but not an infrastructure key, and not the list of everyone.
	if w := r.call(t, "invitation_create", `{"mode":"reusable","ttl":"1w","kind":"infrastructure"}`, nil); w.Code != 403 {
		t.Errorf("user creating an infrastructure key: %d %s", w.Code, w.Body)
	}
	if w := r.call(t, "invitation_list", `{"all":true}`, nil); w.Code != 403 {
		t.Errorf("user listing all: %d %s", w.Code, w.Body)
	}
	var list ops.InvitationListResult
	r.call(t, "invitation_list", `{}`, &list)
	if len(list.Invitations) != 1 || list.Invitations[0].ID != aliceKey.ID || list.Invitations[0].Status != "active" {
		t.Fatalf("alice's list = %+v", list)
	}
	if strings.Contains(r.do("POST", "/api/v1/ops/invitation_list", "{}").Body.String(), aliceKey.Key[strings.Index(aliceKey.Key, ".")+1:]) {
		t.Error("the list leaks the secret")
	}

	// The admin creates an infrastructure key and sees everything.
	r.as("root")
	if w := r.call(t, "invitation_create", `{"mode":"reusable","ttl":"unlimited","kind":"infrastructure"}`, &rootKey); w.Code != 200 || rootKey.Kind != "infrastructure" {
		t.Fatalf("admin create: %d %s", w.Code, w.Body)
	}
	r.call(t, "invitation_list", `{"all":true}`, &list)
	if len(list.Invitations) != 2 {
		t.Errorf("admin list all = %d keys, want 2", len(list.Invitations))
	}
	r.call(t, "invitation_list", `{}`, &list)
	if len(list.Invitations) != 1 || list.Invitations[0].ID != rootKey.ID {
		t.Errorf("admin own list = %+v", list)
	}

	// Another user cannot see or revoke alice's key: not_found.
	r.as("bob")
	r.call(t, "invitation_list", `{}`, &list)
	if len(list.Invitations) != 0 {
		t.Errorf("bob sees %d keys", len(list.Invitations))
	}
	revoke := func(id string) *httptest.ResponseRecorder {
		return r.call(t, "invitation_revoke", `{"request_id":"`+ops.NewUUIDv7()+`","id":"`+id+`"}`, nil)
	}
	if w := revoke(aliceKey.ID); w.Code != 404 {
		t.Errorf("bob revoking alice's key: %d %s", w.Code, w.Body)
	}

	// The creator revokes; the list shows it.
	r.as("alice")
	if w := revoke(aliceKey.ID); w.Code != 200 {
		t.Fatalf("alice revoking: %d %s", w.Code, w.Body)
	}
	r.call(t, "invitation_list", `{}`, &list)
	if list.Invitations[0].Status != "revoked" || !list.Invitations[0].Revoked {
		t.Errorf("after revoke: %+v", list.Invitations[0])
	}
	// The admin may revoke any key.
	r.as("root")
	if w := revoke(rootKey.ID); w.Code != 200 {
		t.Errorf("admin revoke: %d %s", w.Code, w.Body)
	}

	// The audit log has the creations and revocations, no secrets.
	page, err := r.log.List(context.Background(), audit.Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, e := range page.Events {
		actions[e.Action+"/"+e.Outcome]++
		if strings.Contains(e.Target, "mnk_") {
			t.Errorf("audit event leaks a key: %+v", e)
		}
	}
	for want, n := range map[string]int{
		"invitation.create/success": 2, "invitation.revoke/success": 2, "invitation_revoke/success": 2, "invitation_revoke/failed": 1,
	} {
		if actions[want] != n {
			t.Errorf("audit %s = %d, want %d (all: %v)", want, actions[want], n, actions)
		}
	}
}

// Creating a key changes state, so it must not be reachable by GET: a cookie
// session needs no CSRF token on GET, so a cross-site navigation could create
// a key.
func TestInvitationCreateIsNotReachableByGET(t *testing.T) {
	r := newRealRig(t)
	r.as("alice")
	for _, m := range []string{"GET", "HEAD", "PUT", "DELETE"} {
		w := r.do(m, "/api/v1/ops/invitation_create?mode=reusable&ttl=unlimited", "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405: %s", m, w.Code, w.Body)
		}
	}
	rows, err := r.st.ListInvitations(context.Background(), "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("keys after the refused requests = %d err=%v, want 0", len(rows), err)
	}
	if w := r.do("POST", "/api/v1/ops/invitation_create", `{"mode":"reusable","ttl":"1h"}`); w.Code != 200 {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	// Listing stays readable by GET.
	if w := r.do("GET", "/api/v1/ops/invitation_list", ""); w.Code != 200 {
		t.Errorf("GET list: %d %s", w.Code, w.Body)
	}
}

// userScope grants the same actions on the listed users only.
type userScope map[string][]ops.Action

func (s userScope) Actions(res authz.ResolvedResource) []ops.Action { return s[res.UserID] }

var allOps = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}

func (r *realRig) asKey(owner string, scope authz.Scope) {
	r.authn.id = Identity{Principal: authz.NewAPIKeyPrincipal(owner, scope, ""), Actor: owner, Via: ViaAPIKey, IP: testIdentity.IP}
}

// An admin's API key that is scoped to the admin's own user resource must not
// list, revoke or reach infrastructure keys beyond that scope.
func TestScopedAdminAPIKeyKeepsItsResourceScope(t *testing.T) {
	r := newRealRig(t)
	var aliceKey, rootKey ops.InvitationCreateResult
	r.as("alice")
	if w := r.call(t, "invitation_create", `{"mode":"reusable","ttl":"1w"}`, &aliceKey); w.Code != 200 {
		t.Fatal(w.Body)
	}
	r.as("root")
	if w := r.call(t, "invitation_create", `{"mode":"reusable","ttl":"1w"}`, &rootKey); w.Code != 200 {
		t.Fatal(w.Body)
	}
	revoke := func(id string) *httptest.ResponseRecorder {
		return r.call(t, "invitation_revoke", `{"request_id":"`+ops.NewUUIDv7()+`","id":"`+id+`"}`, nil)
	}

	// A key limited to root's own resource.
	r.asKey("root", userScope{"root": allOps})
	var list ops.InvitationListResult
	if w := r.call(t, "invitation_list", `{"all":true}`, &list); w.Code != 200 {
		t.Fatalf("list all: %d %s", w.Code, w.Body)
	}
	if len(list.Invitations) != 1 || list.Invitations[0].ID != rootKey.ID {
		t.Errorf("the scoped key lists %+v, want only root's own key", list.Invitations)
	}
	if w := revoke(aliceKey.ID); w.Code != 404 {
		t.Errorf("revoking another user's key: %d %s, want 404", w.Code, w.Body)
	}
	if w := r.call(t, "invitation_create", `{"mode":"reusable","ttl":"1w","kind":"infrastructure"}`, nil); w.Code != 403 {
		t.Errorf("infrastructure key from a scoped key: %d %s, want 403", w.Code, w.Body)
	}
	if row, _ := r.st.InvitationByID(context.Background(), aliceKey.ID); row.Revoked {
		t.Fatal("alice's key was revoked through the scoped key")
	}
	if w := revoke(rootKey.ID); w.Code != 200 {
		t.Errorf("revoking its own key: %d %s", w.Code, w.Body)
	}

	// A key whose scope also names alice reaches her keys but not bob's.
	r.as("bob")
	var bobKey ops.InvitationCreateResult
	r.call(t, "invitation_create", `{"mode":"reusable","ttl":"1w"}`, &bobKey)
	r.asKey("root", userScope{"root": allOps, "alice": allOps})
	r.call(t, "invitation_list", `{"all":true}`, &list)
	if len(list.Invitations) != 2 {
		t.Errorf("two-user scope lists %d keys, want 2: %+v", len(list.Invitations), list.Invitations)
	}
	for _, i := range list.Invitations {
		if i.ID == bobKey.ID {
			t.Error("the list shows a key of a user outside the scope")
		}
	}
	if w := revoke(bobKey.ID); w.Code != 404 {
		t.Errorf("revoking bob's key: %d, want 404", w.Code)
	}
	if w := revoke(aliceKey.ID); w.Code != 200 {
		t.Errorf("revoking alice's key: %d %s", w.Code, w.Body)
	}

	// An everything scope keeps the full admin reach, infrastructure keys too.
	r.asKey("root", everythingScope{})
	r.call(t, "invitation_list", `{"all":true}`, &list)
	if len(list.Invitations) != 3 {
		t.Errorf("everything scope lists %d keys, want 3", len(list.Invitations))
	}
	if w := revoke(bobKey.ID); w.Code != 200 {
		t.Errorf("everything scope revoking bob's key: %d %s", w.Code, w.Body)
	}
	if w := r.call(t, "invitation_create", `{"mode":"reusable","ttl":"1w","kind":"infrastructure"}`, nil); w.Code != 200 {
		t.Errorf("infrastructure key from an everything scope: %d %s", w.Code, w.Body)
	}
}

type everythingScope struct{}

func (everythingScope) Actions(authz.ResolvedResource) []ops.Action { return allOps }
