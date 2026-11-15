package authz_test

import (
	"context"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// grantStore has an admin-owned controller W1, an alice-owned controller A1,
// alice's Workers W2 and A2 and bob's Worker W3.
func grantStore() *fakeStore {
	st := newStore()
	st.workers = map[string]string{"W1": "admin", "A1": "alice", "A2": "alice", "W2": "alice", "W3": "bob"}
	return st
}

// grantScope builds the stored scope of a session-control grant.
func grantScope(targets ...ops.KeyResource) authz.Scope {
	return apikey.NewScope(targets, authz.GrantActions())
}

var (
	tEverything = ops.KeyResource{Kind: ops.KeyResourceEverything}
	tW2         = ops.KeyResource{Kind: ops.KeyResourceWorker, WorkerID: "W2"}
	tW3Build    = ops.KeyResource{Kind: ops.KeyResourceSession, WorkerID: "W3", SessionName: "build"}
	tA1         = ops.KeyResource{Kind: ops.KeyResourceWorker, WorkerID: "A1"}
)

var (
	rwo = []ops.Action{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}
	rm  = []ops.Action{ops.ActionMessage, ops.ActionRead}
)

func TestGrantPrincipalConstructor(t *testing.T) {
	sc := grantScope(tW2)
	p := authz.NewWorkerGrantPrincipal("W1", sc)
	if p.Kind != authz.KindWorker || p.WorkerID != "W1" || p.Scope != sc || p.UserID != "" {
		t.Errorf("%+v", p)
	}
	if got := authz.GrantActions(); !slices.Equal(got, rwo) {
		t.Errorf("GrantActions = %v", got)
	}
	// The principal is valid: it is accepted by Decide.
	if err := mk(grantStore()).Decide(context.Background(), p, set(ops.ActionRead), authz.WorkerResource{ID: "W2"}); err != nil {
		t.Errorf("valid grant principal denied: %v", err)
	}
}

// TestGrantNoScopeIsUnchanged: a Worker principal without a grant (nil Scope,
// either constructor) holds exactly the own-Worker rule, on every resource.
func TestGrantNoScopeIsUnchanged(t *testing.T) {
	a := mk(grantStore())
	ctx := context.Background()
	all := []authz.Resource{
		authz.WorkerResource{ID: "W1"}, authz.SessionResource{WorkerID: "W1", Name: "boss"},
		authz.WorkerResource{ID: "W2"}, authz.SessionResource{WorkerID: "W2", Name: "x"},
		authz.WorkerResource{ID: "W3"}, authz.SessionResource{WorkerID: "W3", Name: "build"},
		authz.WorkerResource{ID: "A1"}, authz.SessionResource{WorkerID: "A1", Name: "s"},
		authz.UserResource{ID: "admin"}, authz.UserResource{ID: "alice"},
	}
	for _, w := range []string{"W1", "A1"} {
		for _, p := range []authz.Principal{authz.NewWorkerPrincipal(w), authz.NewWorkerGrantPrincipal(w, nil)} {
			for _, res := range all {
				got, err := a.EffectiveActions(ctx, p, res)
				if err != nil {
					t.Fatal(err)
				}
				var want []ops.Action
				if r, ok := res.(authz.WorkerResource); ok && r.ID == w {
					want = rm
				}
				if r, ok := res.(authz.SessionResource); ok && r.WorkerID == w {
					want = rm
				}
				if !slices.Equal([]ops.Action(got), want) {
					t.Errorf("%s %+v: %v, want %v", w, res, got, want)
				}
				if g, err := a.GrantedByScope(ctx, p, set(ops.ActionRead, ops.ActionMessage), res); err != nil || g {
					t.Errorf("%s %+v: granted %v %v", w, res, g, err)
				}
			}
		}
	}
}

func TestGrantDecisions(t *testing.T) {
	type check struct {
		res     authz.Resource
		want    []ops.Action // effective actions
		granted bool         // GrantedByScope for read|operate|message
	}
	w := func(id string) authz.Resource { return authz.WorkerResource{ID: id} }
	s := func(id, name string) authz.Resource { return authz.SessionResource{WorkerID: id, Name: name} }
	cases := []struct {
		name       string
		controller string
		targets    []ops.KeyResource
		checks     []check
	}{
		{"everything, admin owner: all Workers of all owners", "W1", []ops.KeyResource{tEverything}, []check{
			{w("W1"), rwo, true}, {s("W1", "boss"), rwo, true},
			{w("W2"), rwo, true}, {s("W2", "x"), rwo, true},
			{w("W3"), rwo, true}, {s("W3", "other"), rwo, true},
			{w("A2"), rwo, true},
		}},
		{"everything, user owner: only the owner's Workers", "A1", []ops.KeyResource{tEverything}, []check{
			{w("A1"), rwo, true}, {s("A1", "s"), rwo, true},
			{w("A2"), rwo, true}, {s("W2", "x"), rwo, true},
			{w("W1"), nil, false}, {s("W3", "build"), nil, false}, {w("W3"), nil, false},
		}},
		{"Worker W2: W2 and its sessions only", "W1", []ops.KeyResource{tW2}, []check{
			{w("W2"), rwo, true}, {s("W2", "x"), rwo, true}, {s("W2", "y"), rwo, true},
			{w("A2"), nil, false}, {w("W3"), nil, false}, {s("W3", "build"), nil, false},
			{s("W1", "boss"), rm, false}, // own Worker: own rule only, no operate
		}},
		{"session W3:build: that session only", "W1", []ops.KeyResource{tW3Build}, []check{
			{s("W3", "build"), rwo, true},
			{w("W3"), nil, false}, {s("W3", "other"), nil, false}, {s("W2", "build"), nil, false},
		}},
		{"W2 plus W3:build (acceptance 1)", "W1", []ops.KeyResource{tW2, tW3Build}, []check{
			{s("W2", "x"), rwo, true}, {s("W3", "build"), rwo, true}, {s("W3", "other"), nil, false},
		}},
		{"user owner, foreign targets: the owner's limit wins", "A1", []ops.KeyResource{tW3Build, {Kind: ops.KeyResourceWorker, WorkerID: "W1"}}, []check{
			{s("W3", "build"), nil, false}, {w("W1"), nil, false},
		}},
		{"grant on its own Worker adds operate", "A1", []ops.KeyResource{tA1}, []check{
			{w("A1"), rwo, true}, {s("A1", "s"), rwo, true}, {w("A2"), nil, false},
		}},
		{"never user resources beyond the API-key rule, never manage", "A1", []ops.KeyResource{tEverything}, []check{
			{authz.UserResource{ID: "bob"}, nil, false},
		}},
	}
	accepted := set(rwo...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := mk(grantStore())
			ctx := context.Background()
			p := authz.NewWorkerGrantPrincipal(tc.controller, grantScope(tc.targets...))
			for _, c := range tc.checks {
				got, err := a.EffectiveActions(ctx, p, c.res)
				if err != nil || !slices.Equal([]ops.Action(got), c.want) {
					t.Errorf("%+v: effective %v %v, want %v", c.res, got, err, c.want)
				}
				err = a.Decide(ctx, p, accepted, c.res)
				if (err == nil) != (len(c.want) > 0) {
					t.Errorf("%+v: Decide %v", c.res, err)
				}
				if len(c.want) == 0 && code(t, err) != proto.CodeNotFound {
					t.Errorf("%+v: %v, want not_found", c.res, err)
				}
				if c := code(t, a.Decide(ctx, p, set(ops.ActionManage), c.res)); c == "" {
					t.Errorf("manage allowed")
				}
				g, err := a.GrantedByScope(ctx, p, accepted, c.res)
				if err != nil || g != c.granted {
					t.Errorf("%+v: granted %v %v, want %v", c.res, g, err, c.granted)
				}
			}
		})
	}
}

func TestGrantOperateOnlyThroughScope(t *testing.T) {
	a := mk(grantStore())
	ctx := context.Background()
	op := set(ops.ActionOperate)
	own := authz.SessionResource{WorkerID: "A1", Name: "s"}
	// Without a grant and with a grant that does not cover the own Worker,
	// the own-Worker rule never gives operate.
	for _, p := range []authz.Principal{
		authz.NewWorkerPrincipal("A1"),
		authz.NewWorkerGrantPrincipal("A1", grantScope(tW2)),
	} {
		if c := code(t, a.Decide(ctx, p, op, own)); c != proto.CodeForbidden {
			t.Errorf("%+v: operate on own session: %q, want forbidden", p.Scope, c)
		}
		if err := a.Decide(ctx, p, set(ops.ActionMessage), own); err != nil {
			t.Errorf("message on own session: %v", err)
		}
	}
	// A grant whose actions lack operate does not give it either.
	p := authz.NewWorkerGrantPrincipal("A1", apikey.NewScope([]ops.KeyResource{tEverything}, []ops.Action{ops.ActionRead}))
	if c := code(t, a.Decide(ctx, p, op, authz.WorkerResource{ID: "W2"})); c != proto.CodeForbidden {
		t.Errorf("read-only grant: operate %q, want forbidden", c)
	}
	if g, _ := a.GrantedByScope(ctx, p, op, authz.WorkerResource{ID: "W2"}); g {
		t.Error("read-only grant: granted for operate")
	}
	// Only a grant that covers the target gives operate.
	if err := a.Decide(ctx, authz.NewWorkerGrantPrincipal("A1", grantScope(tA1)), op, own); err != nil {
		t.Errorf("granted operate: %v", err)
	}
}

func TestGrantFollowsOwnerStateOnEveryCall(t *testing.T) {
	st := grantStore()
	a := mk(st)
	ctx := context.Background()
	p := authz.NewWorkerGrantPrincipal("W1", grantScope(tEverything))
	foreign := authz.SessionResource{WorkerID: "W3", Name: "build"}
	accepted := set(rwo...)
	if err := a.Decide(ctx, p, accepted, foreign); err != nil {
		t.Fatal(err)
	}
	// Demoted to user: only the owner's own Workers remain.
	st.setRole("admin", authz.RoleUser)
	if c := code(t, a.Decide(ctx, p, accepted, foreign)); c != proto.CodeNotFound {
		t.Errorf("demoted: %q, want not_found", c)
	}
	if err := a.Decide(ctx, p, set(ops.ActionOperate), authz.SessionResource{WorkerID: "W1", Name: "x"}); err != nil {
		t.Errorf("demoted, own Worker: %v", err)
	}
	// Role removed or deactivated: nothing, not even the own Worker.
	st.setRole("admin", "")
	deny := func(label string) {
		t.Helper()
		for _, res := range []authz.Resource{foreign, authz.WorkerResource{ID: "W1"}, authz.SessionResource{WorkerID: "W1", Name: "boss"}} {
			if err := a.Decide(ctx, p, accepted, res); err == nil {
				t.Errorf("%s: %+v allowed", label, res)
			}
			if g, err := a.GrantedByScope(ctx, p, accepted, res); err != nil || g {
				t.Errorf("%s: %+v granted %v %v", label, res, g, err)
			}
		}
		if got, _ := authz.Filter(ctx, a, p, []authz.Resource{foreign, authz.WorkerResource{ID: "W1"}}, func(r authz.Resource) authz.Resource { return r }); len(got) != 0 {
			t.Errorf("%s: Filter %v", label, got)
		}
	}
	deny("role removed")
	st.mu.Lock()
	st.users["admin"] = authz.UserState{Role: authz.RoleAdmin, Active: false}
	st.mu.Unlock()
	deny("deactivated")
	// Reactivated: the grant works again on the next call (nothing cached).
	st.mu.Lock()
	st.users["admin"] = authz.UserState{Role: authz.RoleAdmin, Active: true}
	st.mu.Unlock()
	if err := a.Decide(ctx, p, accepted, foreign); err != nil {
		t.Errorf("reactivated: %v", err)
	}
	// Removed controller Worker: nothing.
	st.mu.Lock()
	delete(st.workers, "W1")
	st.mu.Unlock()
	if err := a.Decide(ctx, p, accepted, foreign); err == nil {
		t.Error("removed controller allowed")
	}
}

func TestGrantedByScopeOnlyForGrants(t *testing.T) {
	a := mk(grantStore())
	ctx := context.Background()
	acc := set(ops.ActionMessage, ops.ActionOperate)
	res := authz.SessionResource{WorkerID: "W2", Name: "x"}
	// An API key, a browser session and a plain Worker principal are allowed
	// here (or on their own Worker) but never count as grants.
	for _, p := range []authz.Principal{
		authz.NewAPIKeyPrincipal("admin", grantScope(tEverything), "W1"),
		authz.NewSessionPrincipal("admin"),
		authz.NewWorkerPrincipal("W2"),
	} {
		if err := a.Decide(ctx, p, acc, res); err != nil {
			t.Fatalf("%s: %v", p.Kind, err)
		}
		if g, err := a.GrantedByScope(ctx, p, acc, res); err != nil || g {
			t.Errorf("%s: granted %v %v", p.Kind, g, err)
		}
	}
	// Unknown resource: false, no error.
	p := authz.NewWorkerGrantPrincipal("W1", grantScope(tEverything))
	if g, err := a.GrantedByScope(ctx, p, acc, authz.WorkerResource{ID: "gone"}); err != nil || g {
		t.Errorf("unknown worker: %v %v", g, err)
	}
	// Own Worker, message accepted: the own-Worker rule allows it, but the
	// grant authorized it too (it covers W1), so provenance is true.
	if g, _ := a.GrantedByScope(ctx, p, set(ops.ActionMessage), authz.WorkerResource{ID: "W1"}); !g {
		t.Error("own Worker covered by the grant: not granted")
	}
	// The grant gives no manage, so an op accepting only manage is not granted.
	if g, _ := a.GrantedByScope(ctx, p, set(ops.ActionManage), res); g {
		t.Error("manage granted")
	}
}

func TestGrantFilter(t *testing.T) {
	a := mk(grantStore())
	ctx := context.Background()
	type item struct{ worker, session string }
	items := []item{{"W1", "boss"}, {"W2", "x"}, {"W3", "build"}, {"W3", "other"}, {"A2", "s"}, {"gone", "s"}}
	res := func(i item) authz.Resource { return authz.SessionResource{WorkerID: i.worker, Name: i.session} }
	cases := []struct {
		name       string
		controller string
		targets    []ops.KeyResource
		want       []item
	}{
		{"no grant: own Worker only", "W1", nil, []item{{"W1", "boss"}}},
		{"everything, admin owner", "W1", []ops.KeyResource{tEverything}, items[:5]},
		{"everything, user owner", "A1", []ops.KeyResource{tEverything}, []item{{"W2", "x"}, {"A2", "s"}}},
		{"W2 + W3:build", "W1", []ops.KeyResource{tW2, tW3Build}, []item{{"W1", "boss"}, {"W2", "x"}, {"W3", "build"}}},
		{"session W3:build", "W1", []ops.KeyResource{tW3Build}, []item{{"W1", "boss"}, {"W3", "build"}}},
	}
	for _, tc := range cases {
		var sc authz.Scope
		if tc.targets != nil {
			sc = grantScope(tc.targets...)
		}
		got, err := authz.Filter(ctx, a, authz.NewWorkerGrantPrincipal(tc.controller, sc), items, res)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v %v, want %v", tc.name, got, err, tc.want)
		}
	}
	// Workers list: everything for an admin owner lists every Worker.
	workers := []string{"W1", "W2", "W3", "A1", "A2"}
	got, _ := authz.Filter(ctx, a, authz.NewWorkerGrantPrincipal("W1", grantScope(tEverything)), workers, func(id string) authz.Resource { return authz.WorkerResource{ID: id} })
	if !slices.Equal(got, workers) {
		t.Errorf("workers: %v", got)
	}
}
