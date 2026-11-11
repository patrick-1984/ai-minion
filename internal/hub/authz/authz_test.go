package authz_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeStore is a mutable UserLookup and WorkerLookup; tests change it between calls.
type fakeStore struct {
	mu      sync.Mutex
	users   map[string]authz.UserState
	workers map[string]string
	calls   int
	fail    error
}

func mk(st *fakeStore, opts ...authz.Option) *authz.Authorizer { return authz.New(st, st, opts...) }

func newStore() *fakeStore {
	return &fakeStore{
		users: map[string]authz.UserState{
			"admin": {Role: authz.RoleAdmin, Active: true},
			"alice": {Role: authz.RoleUser, Active: true},
			"bob":   {Role: authz.RoleUser, Active: true},
		},
		workers: map[string]string{"w-alice": "alice", "w-bob": "bob"},
	}
}

func (s *fakeStore) User(_ context.Context, id string) (authz.UserState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail != nil {
		return authz.UserState{}, s.fail
	}
	u, ok := s.users[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return u, nil
}

func (s *fakeStore) WorkerOwner(_ context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail != nil {
		return "", s.fail
	}
	o, ok := s.workers[id]
	if !ok {
		return "", authz.ErrNotFound
	}
	return o, nil
}

func (s *fakeStore) setRole(id string, r authz.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[id]
	u.Role = r
	s.users[id] = u
}

// stubScope grants fixed actions on resources that match a predicate.
type stubScope struct {
	match   func(authz.ResolvedResource) bool
	actions []ops.Action
}

func (s stubScope) Actions(r authz.ResolvedResource) []ops.Action {
	if s.match(r) {
		return s.actions
	}
	return nil
}

func everything(actions ...ops.Action) authz.Scope {
	return stubScope{match: func(authz.ResolvedResource) bool { return true }, actions: actions}
}

func onWorker(id string, actions ...ops.Action) authz.Scope {
	return stubScope{match: func(r authz.ResolvedResource) bool { return r.WorkerID == id }, actions: actions}
}

var allFour = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}

func set(a ...ops.Action) ops.ActionSet { return ops.MustActionSet(a...) }

func code(t *testing.T, err error) proto.Code {
	t.Helper()
	if err == nil {
		return ""
	}
	var pe *proto.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *proto.Error", err)
	}
	return pe.Code
}

func TestDecideUserOwnResource(t *testing.T) {
	a := mk(newStore())
	alice := authz.NewSessionPrincipal("alice")
	for _, res := range []authz.Resource{
		authz.WorkerResource{ID: "w-alice"},
		authz.SessionResource{WorkerID: "w-alice", Name: "build"},
		authz.UserResource{ID: "alice"},
	} {
		for _, act := range []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage} {
			if err := a.Decide(context.Background(), alice, set(act), res); err != nil {
				t.Errorf("%T %s: %v", res, act, err)
			}
		}
		// manage requires admin: the owner holds other actions, so forbidden.
		if c := code(t, a.Decide(context.Background(), alice, set(ops.ActionManage), res)); c != proto.CodeForbidden {
			t.Errorf("%T manage: code %q, want forbidden", res, c)
		}
	}
}

func TestDecideForeignResource(t *testing.T) {
	a := mk(newStore())
	alice := authz.NewSessionPrincipal("alice")
	for _, res := range []authz.Resource{
		authz.WorkerResource{ID: "w-bob"},
		authz.SessionResource{WorkerID: "w-bob", Name: "x"},
		authz.UserResource{ID: "bob"},
	} {
		for _, act := range allFour {
			// No access at all: not_found, never forbidden (no existence leak).
			if c := code(t, a.Decide(context.Background(), alice, set(act), res)); c != proto.CodeNotFound {
				t.Errorf("%T %s: code %q, want not_found", res, act, c)
			}
		}
	}
	// A missing resource is indistinguishable from a foreign one.
	err := a.Decide(context.Background(), alice, set(ops.ActionRead), authz.WorkerResource{ID: "w-none"})
	if c := code(t, err); c != proto.CodeNotFound {
		t.Errorf("missing worker: %q", c)
	}
}

func TestDecideAdminAllowedEverything(t *testing.T) {
	a := mk(newStore())
	admin := authz.NewSessionPrincipal("admin")
	for _, res := range []authz.Resource{
		authz.WorkerResource{ID: "w-bob"},
		authz.SessionResource{WorkerID: "w-alice", Name: "x"},
		authz.UserResource{ID: "bob"},
	} {
		for _, act := range allFour {
			if err := a.Decide(context.Background(), admin, set(act), res); err != nil {
				t.Errorf("%T %s: %v", res, act, err)
			}
		}
	}
	// Admin on a missing resource is still not_found.
	if c := code(t, a.Decide(context.Background(), admin, set(ops.ActionRead), authz.WorkerResource{ID: "w-none"})); c != proto.CodeNotFound {
		t.Errorf("missing: %q", c)
	}
}

func TestDecideAnyOneAcceptedActionSuffices(t *testing.T) {
	a := mk(newStore())
	ctx := context.Background()
	res := authz.SessionResource{WorkerID: "w-alice", Name: "s"}

	// A key that holds only message passes message|operate but not operate.
	msgKey := authz.NewAPIKeyPrincipal("alice", everything(ops.ActionMessage), "")
	if err := a.Decide(ctx, msgKey, set(ops.ActionMessage, ops.ActionOperate), res); err != nil {
		t.Errorf("message|operate with message: %v", err)
	}
	if c := code(t, a.Decide(ctx, msgKey, set(ops.ActionOperate), res)); c != proto.CodeForbidden {
		t.Errorf("operate with message only: %q, want forbidden", c)
	}
	// operate-only key passes message|operate too.
	opKey := authz.NewAPIKeyPrincipal("alice", everything(ops.ActionOperate), "")
	if err := a.Decide(ctx, opKey, set(ops.ActionMessage, ops.ActionOperate), res); err != nil {
		t.Errorf("message|operate with operate: %v", err)
	}
	// manage does not imply operate for an admin key either: admin holds all
	// four, but a manage-only scope holds just manage.
	mgKey := authz.NewAPIKeyPrincipal("admin", everything(ops.ActionManage), "")
	if c := code(t, a.Decide(ctx, mgKey, set(ops.ActionOperate), res)); c != proto.CodeForbidden {
		t.Errorf("manage must not act as operate: %q", c)
	}
}

func TestEffectiveActions(t *testing.T) {
	st := newStore()
	a := mk(st)
	ctx := context.Background()
	res := authz.SessionResource{WorkerID: "w-alice", Name: "s"}

	cases := []struct {
		name string
		p    authz.Principal
		res  authz.Resource
		want []ops.Action
	}{
		{"owner", authz.NewSessionPrincipal("alice"), res, []ops.Action{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}},
		{"admin", authz.NewSessionPrincipal("admin"), res, []ops.Action{ops.ActionManage, ops.ActionMessage, ops.ActionOperate, ops.ActionRead}},
		{"foreign", authz.NewSessionPrincipal("bob"), res, nil},
		{"scoped read", authz.NewAPIKeyPrincipal("alice", everything(ops.ActionRead), ""), res, []ops.Action{ops.ActionRead}},
		{"missing resource", authz.NewSessionPrincipal("alice"), authz.WorkerResource{ID: "w-none"}, nil},
	}
	for _, c := range cases {
		got, err := a.EffectiveActions(ctx, c.p, c.res)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal([]ops.Action(got), c.want) && (len(got) != 0 || len(c.want) != 0) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScopeIntersectsOwnerPermissions(t *testing.T) {
	st := newStore()
	a := mk(st)
	ctx := context.Background()
	ownW := authz.WorkerResource{ID: "w-alice"}
	otherW := authz.WorkerResource{ID: "w-bob"}

	// A user's key with a wide scope never exceeds what the owner may do.
	wide := authz.NewAPIKeyPrincipal("alice", everything(allFour...), "")
	got, err := a.EffectiveActions(ctx, wide, ownW)
	if err != nil || !slices.Equal([]ops.Action(got), []ops.Action{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}) {
		t.Errorf("own worker with wide scope: %v %v (manage is admin only)", got, err)
	}
	if got, _ := a.EffectiveActions(ctx, wide, otherW); len(got) != 0 {
		t.Errorf("foreign worker with wide scope: %v", got)
	}
	// A narrow scope limits an admin to the scope.
	narrow := authz.NewAPIKeyPrincipal("admin", onWorker("w-bob", ops.ActionRead), "")
	if err := a.Decide(ctx, narrow, set(ops.ActionRead), otherW); err != nil {
		t.Errorf("scoped read on bob's worker: %v", err)
	}
	if c := code(t, a.Decide(ctx, narrow, set(ops.ActionOperate), otherW)); c != proto.CodeForbidden {
		t.Errorf("operate outside scope actions: %q", c)
	}
	if c := code(t, a.Decide(ctx, narrow, set(ops.ActionRead), ownW)); c != proto.CodeNotFound {
		t.Errorf("worker outside scope: %q, want not_found", c)
	}
	// A key without a scope is malformed and denied.
	if err := a.Decide(ctx, authz.NewAPIKeyPrincipal("admin", nil, ""), set(ops.ActionRead), ownW); err == nil {
		t.Error("scopeless key was allowed")
	}
}

func TestNoPositiveCacheOwnerDemoted(t *testing.T) {
	st := newStore()
	a := mk(st)
	ctx := context.Background()
	res := authz.WorkerResource{ID: "w-bob"}

	for name, p := range map[string]authz.Principal{
		"session": authz.NewSessionPrincipal("admin"),
		"api key": authz.NewAPIKeyPrincipal("admin", everything(ops.ActionRead), ""),
		"act-as":  authz.NewActAsPrincipal("admin", "admin", time.Now().Add(time.Hour)),
	} {
		st.setRole("admin", authz.RoleAdmin)
		if err := a.Decide(ctx, p, set(ops.ActionRead), res); err != nil {
			t.Fatalf("%s before demotion: %v", name, err)
		}
		st.setRole("admin", authz.RoleUser) // demoted between two calls
		// A demoted act-as actor is denied at principal level (forbidden); the
		// others lose access to the foreign worker (not_found).
		want := proto.CodeNotFound
		if name == "act-as" {
			want = proto.CodeForbidden
		}
		if c := code(t, a.Decide(ctx, p, set(ops.ActionRead), res)); c != want {
			t.Errorf("%s after demotion: %q, want %s", name, c, want)
		}
	}
	// Reassigning the worker also takes effect on the next call.
	st.setRole("admin", authz.RoleAdmin)
	alice := authz.NewSessionPrincipal("alice")
	aw := authz.WorkerResource{ID: "w-alice"}
	if err := a.Decide(ctx, alice, set(ops.ActionRead), aw); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.workers["w-alice"] = "bob"
	st.mu.Unlock()
	if c := code(t, a.Decide(ctx, alice, set(ops.ActionRead), aw)); c != proto.CodeNotFound {
		t.Errorf("after reassignment: %q", c)
	}
}

func TestDeactivatedAndUnknownUsersAreDenied(t *testing.T) {
	st := newStore()
	a := mk(st)
	ctx := context.Background()
	res := authz.WorkerResource{ID: "w-alice"}
	st.users["alice"] = authz.UserState{Role: authz.RoleUser, Active: false}
	if c := code(t, a.Decide(ctx, authz.NewSessionPrincipal("alice"), set(ops.ActionRead), res)); c != proto.CodeForbidden {
		t.Errorf("deactivated: %q", c)
	}
	if err := a.Decide(ctx, authz.NewSessionPrincipal("ghost"), set(ops.ActionRead), res); err == nil {
		t.Error("unknown user allowed")
	}
	st.users["weird"] = authz.UserState{Role: "root", Active: true}
	st.workers["w-weird"] = "weird"
	if err := a.Decide(ctx, authz.NewSessionPrincipal("weird"), set(ops.ActionRead), authz.WorkerResource{ID: "w-weird"}); err == nil {
		t.Error("unknown role allowed")
	}
	if err := a.Decide(ctx, authz.Principal{}, set(ops.ActionRead), res); err == nil {
		t.Error("zero principal allowed")
	}
}

func TestActAs(t *testing.T) {
	st := newStore()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	a := mk(st, authz.WithClock(func() time.Time { return now }))
	ctx := context.Background()
	read := set(ops.ActionRead)
	exp := now.Add(time.Hour)

	p := authz.NewActAsPrincipal("admin", "alice", exp)
	// Acts with the target's permissions, not the admin's.
	if err := a.Decide(ctx, p, read, authz.WorkerResource{ID: "w-alice"}); err != nil {
		t.Errorf("target's worker: %v", err)
	}
	if c := code(t, a.Decide(ctx, p, read, authz.WorkerResource{ID: "w-bob"})); c != proto.CodeNotFound {
		t.Errorf("other user's worker: %q", c)
	}
	if c := code(t, a.Decide(ctx, p, set(ops.ActionManage), authz.WorkerResource{ID: "w-alice"})); c != proto.CodeForbidden {
		t.Errorf("manage while acting as user: %q", c)
	}
	// Expired.
	now = exp
	if err := a.Decide(ctx, p, read, authz.WorkerResource{ID: "w-alice"}); err == nil {
		t.Error("expired act-as allowed")
	}
	now = exp.Add(-time.Minute)
	// The actor must still be an admin.
	st.setRole("admin", authz.RoleUser)
	if err := a.Decide(ctx, p, read, authz.WorkerResource{ID: "w-alice"}); err == nil {
		t.Error("act-as by a demoted actor allowed")
	}
	st.setRole("admin", authz.RoleAdmin)
	// Acting as a non-admin: a plain user cannot act as anyone.
	if err := a.Decide(ctx, authz.NewActAsPrincipal("bob", "alice", exp), read, authz.WorkerResource{ID: "w-alice"}); err == nil {
		t.Error("non-admin actor allowed")
	}
	// Target gone.
	if err := a.Decide(ctx, authz.NewActAsPrincipal("admin", "ghost", exp), read, authz.WorkerResource{ID: "w-alice"}); err == nil {
		t.Error("unknown target allowed")
	}
}

func TestShareOpsOperateOnWorker(t *testing.T) {
	// A-8: share_get/share_set accept `operate` on the Worker, through
	// resource ownership and not through the global manage action.
	a := mk(newStore())
	ctx := context.Background()
	accepted := set(ops.ActionOperate)
	w := authz.WorkerResource{ID: "w-alice"}

	if err := a.Decide(ctx, authz.NewSessionPrincipal("alice"), accepted, w); err != nil {
		t.Errorf("owner: %v", err)
	}
	if c := code(t, a.Decide(ctx, authz.NewSessionPrincipal("bob"), accepted, w)); c != proto.CodeNotFound {
		t.Errorf("foreign owner: %q", c)
	}
	if err := a.Decide(ctx, authz.NewSessionPrincipal("admin"), accepted, w); err != nil {
		t.Errorf("admin: %v", err)
	}
	// API key: needs operate scope on that Worker.
	key := authz.NewAPIKeyPrincipal("alice", onWorker("w-alice", ops.ActionOperate), "")
	if err := a.Decide(ctx, key, accepted, w); err != nil {
		t.Errorf("key with operate scope: %v", err)
	}
	for name, scope := range map[string]authz.Scope{
		"read only":    onWorker("w-alice", ops.ActionRead),
		"message only": onWorker("w-alice", ops.ActionMessage),
		"manage only":  onWorker("w-alice", ops.ActionManage),
		"other worker": onWorker("w-bob", ops.ActionOperate),
	} {
		if err := a.Decide(ctx, authz.NewAPIKeyPrincipal("alice", scope, ""), accepted, w); err == nil {
			t.Errorf("key with %s scope was allowed", name)
		}
	}
	// An admin key with a manage-only scope does not pass: manage is not operate.
	if err := a.Decide(ctx, authz.NewAPIKeyPrincipal("admin", onWorker("w-alice", ops.ActionManage), ""), accepted, w); err == nil {
		t.Error("manage scope acted as operate")
	}
}

func TestWorkerBoundKeyIsLimitedByScopeAndOwnerNotByResource(t *testing.T) {
	st := newStore()
	st.workers["w-alice2"] = "alice"
	a := mk(st)
	ctx := context.Background()
	other := authz.SessionResource{WorkerID: "w-alice2", Name: "s"}

	// A key of alice bound to Worker A reaches a session on another Worker of
	// alice when its scope allows it (Story 7.4: pipe_send across Workers).
	key := authz.NewAPIKeyPrincipal("alice", everything(ops.ActionMessage, ops.ActionRead), "w-alice")
	if err := a.Decide(ctx, key, set(ops.ActionMessage, ops.ActionOperate), other); err != nil {
		t.Errorf("bound key, other worker, in scope: %v", err)
	}
	// Scope still limits the actions ...
	if c := code(t, a.Decide(ctx, key, set(ops.ActionOperate), other)); c != proto.CodeForbidden {
		t.Errorf("operate outside scope actions: %q, want forbidden", c)
	}
	// ... and the resources it covers.
	narrow := authz.NewAPIKeyPrincipal("alice", onWorker("w-alice", ops.ActionMessage), "w-alice")
	if c := code(t, a.Decide(ctx, narrow, set(ops.ActionMessage), other)); c != proto.CodeNotFound {
		t.Errorf("worker outside scope: %q, want not_found", c)
	}
	if err := a.Decide(ctx, narrow, set(ops.ActionMessage), authz.SessionResource{WorkerID: "w-alice", Name: "s"}); err != nil {
		t.Errorf("bound worker in scope: %v", err)
	}
	// The owner's permissions still cap the key.
	if c := code(t, a.Decide(ctx, key, set(ops.ActionMessage), authz.SessionResource{WorkerID: "w-bob", Name: "s"})); c != proto.CodeNotFound {
		t.Errorf("foreign owner's session: %q, want not_found", c)
	}
	wide := authz.NewAPIKeyPrincipal("alice", everything(allFour...), "w-alice")
	got, err := a.EffectiveActions(ctx, wide, other)
	if err != nil || !slices.Equal([]ops.Action(got), []ops.Action{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}) {
		t.Errorf("wide bound key: %v %v (manage is admin only)", got, err)
	}
	// An admin-owned bound key with a wide scope reaches everything, like any key.
	adminKey := authz.NewAPIKeyPrincipal("admin", everything(allFour...), "w-alice")
	for _, res := range []authz.Resource{authz.WorkerResource{ID: "w-bob"}, authz.UserResource{ID: "alice"}} {
		if err := a.Decide(ctx, adminKey, set(ops.ActionRead), res); err != nil {
			t.Errorf("admin bound key on %T: %v", res, err)
		}
	}
	// The owner's current state applies: a deactivated owner loses the key.
	st.mu.Lock()
	st.users["alice"] = authz.UserState{Role: authz.RoleUser, Active: false}
	st.mu.Unlock()
	if err := a.Decide(ctx, key, set(ops.ActionMessage), other); err == nil {
		t.Error("bound key of a deactivated owner was allowed")
	}
}

func TestWorkerPrincipal(t *testing.T) {
	st := newStore()
	st.workers["w-alice2"] = "alice"
	a := mk(st)
	ctx := context.Background()
	w := authz.NewWorkerPrincipal("w-alice")
	own := authz.SessionResource{WorkerID: "w-alice", Name: "s"}
	peer := authz.SessionResource{WorkerID: "w-alice2", Name: "s"}

	// F-01: a same-owner peer Worker and its sessions are not reachable.
	for _, res := range []authz.Resource{peer, authz.WorkerResource{ID: "w-alice2"}} {
		for _, act := range []ops.Action{ops.ActionRead, ops.ActionMessage} {
			if c := code(t, a.Decide(ctx, w, set(act), res)); c != proto.CodeNotFound {
				t.Errorf("peer %T %+v %s: %q, want not_found", res, res, act, c)
			}
		}
		if got, err := a.EffectiveActions(ctx, w, res); err != nil || len(got) != 0 {
			t.Errorf("peer %+v effective: %v %v", res, got, err)
		}
	}
	// Own Worker and its sessions: read and message.
	for _, res := range []authz.Resource{own, authz.WorkerResource{ID: "w-alice"}} {
		for _, act := range []ops.Action{ops.ActionRead, ops.ActionMessage} {
			if err := a.Decide(ctx, w, set(act), res); err != nil {
				t.Errorf("%T %+v %s: %v", res, res, act, err)
			}
		}
		// Call-stream pipe_send accepts message|operate.
		if err := a.Decide(ctx, w, set(ops.ActionMessage, ops.ActionOperate), res); err != nil {
			t.Errorf("%+v message|operate: %v", res, err)
		}
		// operate and manage are never held: forbidden, not not_found.
		for _, act := range []ops.Action{ops.ActionOperate, ops.ActionManage} {
			if c := code(t, a.Decide(ctx, w, set(act), res)); c != proto.CodeForbidden {
				t.Errorf("%+v %s: %q, want forbidden", res, act, c)
			}
		}
		got, err := a.EffectiveActions(ctx, w, res)
		if err != nil || !slices.Equal([]ops.Action(got), []ops.Action{ops.ActionMessage, ops.ActionRead}) {
			t.Errorf("%+v effective: %v %v", res, got, err)
		}
	}
	// A foreign owner's Worker or session: not_found.
	for _, res := range []authz.Resource{authz.WorkerResource{ID: "w-bob"}, authz.SessionResource{WorkerID: "w-bob", Name: "x"}} {
		if c := code(t, a.Decide(ctx, w, set(ops.ActionRead), res)); c != proto.CodeNotFound {
			t.Errorf("%+v: %q, want not_found", res, c)
		}
	}
	// Nothing on user resources, not even the owner's.
	for _, id := range []string{"alice", "bob"} {
		if err := a.Decide(ctx, w, set(ops.ActionRead), authz.UserResource{ID: id}); err == nil {
			t.Errorf("worker reached user resource %s", id)
		}
	}
	// Filter lists only the Worker's own Worker and sessions.
	items := []authz.Resource{own, peer, authz.WorkerResource{ID: "w-alice"}, authz.WorkerResource{ID: "w-bob"}}
	listed, err := authz.Filter(ctx, a, w, items, func(r authz.Resource) authz.Resource { return r })
	if err != nil || len(listed) != 2 {
		t.Errorf("Filter: %v %v", listed, err)
	}
}

func TestWorkerPrincipalOfAdminOwnerHasNoHubWideReach(t *testing.T) {
	st := newStore()
	st.workers["w-infra"] = "admin"
	a := mk(st)
	ctx := context.Background()
	w := authz.NewWorkerPrincipal("w-infra")
	if err := a.Decide(ctx, w, set(ops.ActionRead), authz.SessionResource{WorkerID: "w-infra", Name: "s"}); err != nil {
		t.Errorf("admin-owned own session: %v", err)
	}
	for _, res := range []authz.Resource{authz.WorkerResource{ID: "w-bob"}, authz.SessionResource{WorkerID: "w-alice", Name: "s"}} {
		if c := code(t, a.Decide(ctx, w, set(ops.ActionRead), res)); c != proto.CodeNotFound {
			t.Errorf("%+v: %q, want not_found", res, c)
		}
	}
	if c := code(t, a.Decide(ctx, w, set(ops.ActionManage), authz.WorkerResource{ID: "w-infra"})); c != proto.CodeForbidden {
		t.Errorf("manage: %q, want forbidden", c)
	}
}

func TestWorkerPrincipalIsResolvedFreshOnEveryCall(t *testing.T) {
	st := newStore()
	st.workers["w-alice2"] = "alice"
	a := mk(st)
	ctx := context.Background()
	w := authz.NewWorkerPrincipal("w-alice")
	peer := authz.SessionResource{WorkerID: "w-alice", Name: "s"}
	msg := set(ops.ActionMessage, ops.ActionOperate)
	if err := a.Decide(ctx, w, msg, peer); err != nil {
		t.Fatal(err)
	}

	// Deactivated owner.
	st.mu.Lock()
	st.users["alice"] = authz.UserState{Role: authz.RoleUser, Active: false}
	st.mu.Unlock()
	if c := code(t, a.Decide(ctx, w, msg, peer)); c != proto.CodeForbidden {
		t.Errorf("deactivated owner: %q, want forbidden", c)
	}
	// Missing owner.
	st.mu.Lock()
	delete(st.users, "alice")
	st.mu.Unlock()
	if c := code(t, a.Decide(ctx, w, msg, peer)); c != proto.CodeForbidden {
		t.Errorf("missing owner: %q, want forbidden", c)
	}
	st.mu.Lock()
	st.users["alice"] = authz.UserState{Role: authz.RoleUser, Active: true}
	st.mu.Unlock()
	if err := a.Decide(ctx, w, msg, peer); err != nil {
		t.Fatalf("reactivated: %v", err)
	}

	// Reassigning the Worker changes the result on the next call.
	st.mu.Lock()
	st.workers["w-alice"] = "bob"
	st.mu.Unlock()
	if err := a.Decide(ctx, w, msg, peer); err != nil {
		t.Errorf("own session after reassignment follows the Worker: %v", err)
	}
	if c := code(t, a.Decide(ctx, w, msg, authz.SessionResource{WorkerID: "w-alice2", Name: "s"})); c != proto.CodeNotFound {
		t.Errorf("former sibling: %q, want not_found", c)
	}

	// Removed Worker.
	st.mu.Lock()
	delete(st.workers, "w-alice")
	st.mu.Unlock()
	if c := code(t, a.Decide(ctx, w, msg, peer)); c != proto.CodeForbidden {
		t.Errorf("removed worker: %q, want forbidden", c)
	}
	if got, err := a.EffectiveActions(ctx, w, peer); err != nil || len(got) != 0 {
		t.Errorf("removed worker effective: %v %v", got, err)
	}
}

func TestWorkerPrincipalWorkerLookupFailureDenies(t *testing.T) {
	boom := errors.New("db down")
	st := newStore()
	a := authz.New(st, &failingWorkers{fakeStore: st, err: boom})
	err := a.Decide(context.Background(), authz.NewWorkerPrincipal("w-alice"), set(ops.ActionRead), authz.UserResource{ID: "alice"})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the lookup error", err)
	}
}

func TestUnwiredAuthorizerFailsClosedAndWiresOnce(t *testing.T) {
	a := authz.NewUnwired()
	ctx := context.Background()
	p := authz.NewSessionPrincipal("alice")
	res := authz.WorkerResource{ID: "w-alice"}
	if err := a.Decide(ctx, p, set(ops.ActionRead), res); !errors.Is(err, authz.ErrNotWired) {
		t.Errorf("Decide = %v", err)
	}
	st := newStore()
	if err := a.SetUserLookup(st); err != nil {
		t.Fatal(err)
	}
	if err := a.SetUserLookup(st); err == nil {
		t.Error("second SetUserLookup accepted")
	}
	if err := a.Decide(ctx, p, set(ops.ActionRead), res); !errors.Is(err, authz.ErrNotWired) {
		t.Errorf("Decide with worker port unwired = %v", err)
	}
	if err := a.SetWorkerLookup(st); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkerLookup(st); err == nil {
		t.Error("second SetWorkerLookup accepted")
	}
	if err := a.Decide(ctx, p, set(ops.ActionRead), res); err != nil {
		t.Errorf("Decide after wiring = %v", err)
	}
	// A New authorizer is fully wired already.
	if err := mk(st).SetUserLookup(st); err == nil {
		t.Error("SetUserLookup on a wired authorizer accepted")
	}
}

func TestWiringIsSafeConcurrentlyWithDecisions(t *testing.T) {
	a := authz.NewUnwired()
	st := newStore()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = a.Decide(context.Background(), authz.NewSessionPrincipal("alice"), set(ops.ActionRead), authz.WorkerResource{ID: "w-alice"})
			}
		}()
	}
	_ = a.SetUserLookup(st)
	_ = a.SetWorkerLookup(st)
	wg.Wait()
}

func TestLookupFailureDenies(t *testing.T) {
	st := newStore()
	a := mk(st)
	ctx := context.Background()
	boom := errors.New("db down")
	st.fail = boom
	p := authz.NewSessionPrincipal("admin")
	res := authz.WorkerResource{ID: "w-alice"}
	if err := a.Decide(ctx, p, set(ops.ActionRead), res); !errors.Is(err, boom) {
		t.Errorf("Decide err = %v, want the lookup error", err)
	}
	if _, err := a.EffectiveActions(ctx, p, res); !errors.Is(err, boom) {
		t.Errorf("EffectiveActions err = %v", err)
	}
	if _, err := authz.Filter(ctx, a, p, []string{"w-alice"}, func(id string) authz.Resource { return authz.WorkerResource{ID: id} }); !errors.Is(err, boom) {
		t.Errorf("Filter err = %v", err)
	}
	// Failure while resolving the resource only (the principal resolved).
	st.fail = nil
	st2 := &failingWorkers{fakeStore: newStore(), err: boom}
	a2 := authz.New(st2.fakeStore, st2)
	if err := a2.Decide(ctx, p, set(ops.ActionRead), res); !errors.Is(err, boom) {
		t.Errorf("worker lookup failure: %v", err)
	}
}

type failingWorkers struct {
	*fakeStore
	err error
}

func (f *failingWorkers) WorkerOwner(context.Context, string) (string, error) { return "", f.err }

func TestFilter(t *testing.T) {
	st := newStore()
	st.workers["w-alice2"] = "alice"
	a := mk(st)
	ctx := context.Background()
	type item struct{ worker, session string }
	items := []item{
		{"w-alice", "a"}, {"w-bob", "b"}, {"w-alice2", "c"}, {"w-gone", "d"}, {"w-bob", "e"},
	}
	res := func(i item) authz.Resource { return authz.SessionResource{WorkerID: i.worker, Name: i.session} }

	got, err := authz.Filter(ctx, a, authz.NewSessionPrincipal("alice"), items, res)
	if err != nil {
		t.Fatal(err)
	}
	if want := []item{{"w-alice", "a"}, {"w-alice2", "c"}}; !slices.Equal(got, want) {
		t.Errorf("user: %v, want %v", got, want)
	}
	got, _ = authz.Filter(ctx, a, authz.NewSessionPrincipal("admin"), items, res)
	if len(got) != 4 { // the item of the deleted worker is dropped
		t.Errorf("admin: %v", got)
	}
	// Scope narrows the admin; a scope without read removes everything.
	got, _ = authz.Filter(ctx, a, authz.NewAPIKeyPrincipal("admin", onWorker("w-bob", ops.ActionRead), ""), items, res)
	if want := []item{{"w-bob", "b"}, {"w-bob", "e"}}; !slices.Equal(got, want) {
		t.Errorf("scoped admin: %v", got)
	}
	got, _ = authz.Filter(ctx, a, authz.NewAPIKeyPrincipal("admin", everything(ops.ActionOperate), ""), items, res)
	if len(got) != 0 {
		t.Errorf("operate-only scope lists %v", got)
	}
	got, err = authz.Filter(ctx, a, authz.NewSessionPrincipal("ghost"), items, res)
	if err != nil || len(got) != 0 {
		t.Errorf("unknown user: %v %v", got, err)
	}
	// The input is not modified, and an empty input is fine.
	if len(items) != 5 || items[1].worker != "w-bob" {
		t.Error("input modified")
	}
	if got, _ := authz.Filter(ctx, a, authz.NewSessionPrincipal("alice"), []item(nil), res); got == nil || len(got) != 0 {
		t.Errorf("empty input: %#v", got)
	}
	// User list: a user sees only themselves.
	users, _ := authz.Filter(ctx, a, authz.NewSessionPrincipal("alice"), []string{"admin", "alice", "bob"}, func(id string) authz.Resource { return authz.UserResource{ID: id} })
	if !slices.Equal(users, []string{"alice"}) {
		t.Errorf("users: %v", users)
	}
}

func TestFilterReadsOwnersFreshPerCallAndMemoisesWithinOne(t *testing.T) {
	st := newStore()
	a := mk(st)
	ctx := context.Background()
	items := make([]string, 50)
	for i := range items {
		items[i] = "w-alice"
	}
	res := func(id string) authz.Resource { return authz.WorkerResource{ID: id} }
	alice := authz.NewSessionPrincipal("alice")

	st.calls = 0
	if got, _ := authz.Filter(ctx, a, alice, items, res); len(got) != 50 {
		t.Fatalf("got %d", len(got))
	}
	if st.calls != 2 { // one user read, one owner read for 50 items
		t.Errorf("store calls = %d, want 2", st.calls)
	}
	st.mu.Lock()
	st.workers["w-alice"] = "bob"
	st.mu.Unlock()
	if got, _ := authz.Filter(ctx, a, alice, items, res); len(got) != 0 {
		t.Errorf("stale owner served: %d items", len(got))
	}
}

func TestPrincipalConstructors(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	if p := authz.NewSessionPrincipal("u"); p.Kind != authz.KindBrowserSession || p.UserID != "u" {
		t.Errorf("%+v", p)
	}
	if p := authz.NewAPIKeyPrincipal("u", everything(), "w"); p.Kind != authz.KindAPIKey || p.WorkerID != "w" || p.Scope == nil {
		t.Errorf("%+v", p)
	}
	if p := authz.NewActAsPrincipal("a", "t", exp); p.Kind != authz.KindActAs || p.ActorID != "a" || p.UserID != "t" || !p.ActAsExpires.Equal(exp) {
		t.Errorf("%+v", p)
	}
	if p := authz.NewWorkerPrincipal("w"); p.Kind != authz.KindWorker || p.WorkerID != "w" {
		t.Errorf("%+v", p)
	}
	a := mk(newStore())
	for name, p := range map[string]authz.Principal{
		"session no user": authz.NewSessionPrincipal(""),
		"act-as no exp":   authz.NewActAsPrincipal("admin", "alice", time.Time{}),
		"act-as no actor": authz.NewActAsPrincipal("", "alice", exp),
		"worker no id":    authz.NewWorkerPrincipal(""),
		"unknown kind":    {Kind: "x", UserID: "admin"},
	} {
		if err := a.Decide(context.Background(), p, set(ops.ActionRead), authz.WorkerResource{ID: "w-alice"}); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestIncompleteResourcesAreNotFound(t *testing.T) {
	a := mk(newStore())
	for _, res := range []authz.Resource{
		authz.UserResource{}, authz.WorkerResource{}, authz.SessionResource{WorkerID: "w-alice"}, authz.SessionResource{Name: "x"},
	} {
		err := a.Decide(context.Background(), authz.NewSessionPrincipal("admin"), set(ops.ActionRead), res)
		if c := code(t, err); c != proto.CodeNotFound {
			t.Errorf("%#v: %q", res, c)
		}
	}
}

func TestNewPanicsWithoutLookup(t *testing.T) {
	st := newStore()
	for name, f := range map[string]func(){
		"no users":   func() { authz.New(nil, st) },
		"no workers": func() { authz.New(st, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: New did not panic", name)
				}
			}()
			f()
		}()
	}
}
