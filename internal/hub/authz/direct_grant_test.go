package authz_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var (
	dgNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	ro    = []ops.Action{ops.ActionRead}
)

// ownerGrant is a direct grant between two Workers of alice.
func ownerGrant(id, worker, session string, level int) authz.DirectGrant {
	return authz.DirectGrant{
		ID: id, TargetWorkerID: worker, TargetSession: session, Level: level,
		Authority: authz.AuthorityOwner, GrantedBy: "alice", SourceOwnerID: "alice", TargetOwnerID: "alice",
	}
}

// adminGrant is a direct grant from a Worker of alice to a Worker of bob,
// decided by the administrator.
func adminGrant(id, worker, session string, level int) authz.DirectGrant {
	return authz.DirectGrant{
		ID: id, TargetWorkerID: worker, TargetSession: session, Level: level,
		Authority: authz.AuthorityAdmin, GrantedBy: "admin", SourceOwnerID: "alice", TargetOwnerID: "bob",
	}
}

func dgAuthz(st *fakeStore) *authz.Authorizer {
	return mk(st, authz.WithClock(func() time.Time { return dgNow }))
}

func sess(worker, name string) authz.Resource {
	return authz.SessionResource{WorkerID: worker, Name: name}
}

func TestLevelActions(t *testing.T) {
	want := map[int][]ops.Action{
		0: nil, 1: {ops.ActionRead}, 2: {ops.ActionRead, ops.ActionMessage},
		3: {ops.ActionRead, ops.ActionMessage, ops.ActionOperate}, 4: nil, -1: nil,
	}
	for level, w := range want {
		if got := authz.LevelActions(level); !slices.Equal(got, w) {
			t.Errorf("LevelActions(%d) = %v, want %v", level, got, w)
		}
	}
}

func TestWorkerSessionPrincipalConstructor(t *testing.T) {
	g := []authz.DirectGrant{ownerGrant("g1", "W2", "x", 1)}
	p := authz.NewWorkerSessionPrincipal("A1", "main", nil, g)
	if p.Kind != authz.KindWorker || p.WorkerID != "A1" || p.Scope != nil || p.UserID != "" ||
		p.SessionName() != "main" || !slices.Equal(p.DirectGrants(), g) {
		t.Errorf("%+v", p)
	}
	if ok, err := dgAuthz(grantStore()).Valid(context.Background(), p); err != nil || !ok {
		t.Errorf("Valid = %v %v", ok, err)
	}
	// Without a session and without grants it is still a valid Worker identity.
	if ok, err := dgAuthz(grantStore()).Valid(context.Background(), authz.NewWorkerSessionPrincipal("A1", "", nil, nil)); err != nil || !ok {
		t.Errorf("Valid (empty) = %v %v", ok, err)
	}
}

// TestDirectGrantDecisions is the decision table for the caller A1 (owner
// alice, a plain user). mutate changes the store before the checks.
func TestDirectGrantDecisions(t *testing.T) {
	type check struct {
		res  authz.Resource
		want []ops.Action
		ids  []string // GrantIDs for read|message|operate
	}
	expired, live, suspended := ownerGrant("g", "W2", "x", 3), ownerGrant("g", "W2", "x", 3), ownerGrant("g", "W2", "x", 3)
	expired.ExpiresAt = dgNow
	live.ExpiresAt = dgNow.Add(time.Second)
	suspended.Suspended = true
	foreignSource := ownerGrant("g", "W2", "x", 3)
	foreignSource.SourceOwnerID, foreignSource.TargetOwnerID = "bob", "bob"
	crossOwner := ownerGrant("g", "W3", "build", 3)
	crossOwner.TargetOwnerID = "bob"
	badAuthority := ownerGrant("g", "W2", "x", 3)
	badAuthority.Authority = ""
	cases := []struct {
		name   string
		grants []authz.DirectGrant
		mutate func(*fakeStore)
		checks []check
	}{
		{"owner authority, view", []authz.DirectGrant{ownerGrant("g", "W2", "x", 1)}, nil, []check{
			{sess("W2", "x"), ro, []string{"g"}}, {sess("W2", "y"), nil, nil},
		}},
		{"owner authority, message", []authz.DirectGrant{ownerGrant("g", "W2", "x", 2)}, nil, []check{
			{sess("W2", "x"), rm, []string{"g"}},
		}},
		{"owner authority, control", []authz.DirectGrant{ownerGrant("g", "W2", "x", 3)}, nil, []check{
			{sess("W2", "x"), rwo, []string{"g"}},
		}},
		{"unknown level", []authz.DirectGrant{ownerGrant("g", "W2", "x", 4)}, nil, []check{
			{sess("W2", "x"), nil, nil}, {authz.WorkerResource{ID: "W2"}, nil, nil},
		}},
		{"wildcard target: every session of that Worker only", []authz.DirectGrant{ownerGrant("g", "W2", "", 3)}, nil, []check{
			{sess("W2", "x"), rwo, []string{"g"}}, {sess("W2", "later"), rwo, []string{"g"}},
			{sess("A2", "s"), nil, nil}, {sess("W3", "build"), nil, nil},
		}},
		{"Worker resource: read only; user resource: nothing", []authz.DirectGrant{ownerGrant("g", "W2", "x", 3)}, nil, []check{
			{authz.WorkerResource{ID: "W2"}, ro, []string{"g"}}, {authz.WorkerResource{ID: "A2"}, nil, nil},
			{authz.UserResource{ID: "alice"}, nil, nil}, {authz.UserResource{ID: "bob"}, nil, nil},
		}},
		{"own Worker rule stays", []authz.DirectGrant{ownerGrant("g", "W2", "x", 1)}, nil, []check{
			{sess("A1", "s"), rm, nil}, {authz.WorkerResource{ID: "A1"}, rm, nil},
		}},
		{"union of two grants", []authz.DirectGrant{ownerGrant("g1", "W2", "", 1), ownerGrant("g2", "W2", "x", 3)}, nil, []check{
			{sess("W2", "x"), rwo, []string{"g1", "g2"}}, {sess("W2", "y"), ro, []string{"g1"}},
		}},
		{"admin authority across owners, calling owner is a plain user", []authz.DirectGrant{adminGrant("g", "W3", "build", 3)}, nil, []check{
			{sess("W3", "build"), rwo, []string{"g"}}, {sess("W3", "other"), nil, nil},
			{authz.WorkerResource{ID: "W3"}, ro, []string{"g"}},
		}},
		{"grantor demoted", []authz.DirectGrant{adminGrant("g", "W3", "build", 3)},
			func(st *fakeStore) { st.setRole("admin", authz.RoleUser) }, []check{
				{sess("W3", "build"), nil, nil}, {authz.WorkerResource{ID: "W3"}, nil, nil},
			}},
		{"grantor deactivated", []authz.DirectGrant{adminGrant("g", "W3", "build", 3)},
			func(st *fakeStore) { st.users["admin"] = authz.UserState{Role: authz.RoleAdmin} }, []check{
				{sess("W3", "build"), nil, nil},
			}},
		{"grantor removed", []authz.DirectGrant{adminGrant("g", "W3", "build", 3)},
			func(st *fakeStore) { delete(st.users, "admin") }, []check{
				{sess("W3", "build"), nil, nil},
			}},
		{"admin authority, target changed owner", []authz.DirectGrant{adminGrant("g", "W3", "build", 3)},
			func(st *fakeStore) { st.workers["W3"] = "alice" }, []check{
				{sess("W3", "build"), nil, nil},
			}},
		{"owner authority, target changed owner", []authz.DirectGrant{ownerGrant("g", "W2", "x", 3)},
			func(st *fakeStore) { st.workers["W2"] = "bob" }, []check{
				{sess("W2", "x"), nil, nil}, {authz.WorkerResource{ID: "W2"}, nil, nil},
			}},
		{"owner authority, calling Worker changed owner", []authz.DirectGrant{ownerGrant("g", "W2", "x", 3)},
			func(st *fakeStore) { st.workers["A1"] = "bob" }, []check{
				{sess("W2", "x"), nil, nil},
			}},
		{"owner authority, source owner is not the calling owner", []authz.DirectGrant{foreignSource}, nil, []check{
			{sess("W2", "x"), nil, nil},
		}},
		{"owner authority across owners is not valid", []authz.DirectGrant{crossOwner}, nil, []check{
			{sess("W3", "build"), nil, nil},
		}},
		{"unknown authority", []authz.DirectGrant{badAuthority}, nil, []check{{sess("W2", "x"), nil, nil}}},
		{"expired", []authz.DirectGrant{expired}, nil, []check{{sess("W2", "x"), nil, nil}}},
		{"not yet expired", []authz.DirectGrant{live}, nil, []check{{sess("W2", "x"), rwo, []string{"g"}}}},
		{"suspended", []authz.DirectGrant{suspended}, nil, []check{
			{sess("W2", "x"), nil, nil}, {authz.WorkerResource{ID: "W2"}, nil, nil},
		}},
	}
	ctx := context.Background()
	any3 := set(ops.ActionRead, ops.ActionMessage, ops.ActionOperate)
	for _, tc := range cases {
		st := grantStore()
		if tc.mutate != nil {
			tc.mutate(st)
		}
		a := dgAuthz(st)
		p := authz.NewWorkerSessionPrincipal("A1", "main", nil, tc.grants)
		for _, c := range tc.checks {
			got, err := a.EffectiveActions(ctx, p, c.res)
			if err != nil || !slices.Equal([]ops.Action(got), c.want) {
				t.Errorf("%s %+v: %v %v, want %v", tc.name, c.res, got, err, c.want)
			}
			ids, err := a.GrantIDs(ctx, p, any3, c.res)
			if err != nil || !slices.Equal(ids, c.ids) {
				t.Errorf("%s %+v: GrantIDs %v %v, want %v", tc.name, c.res, ids, err, c.ids)
			}
			if g, err := a.GrantedByScope(ctx, p, any3, c.res); err != nil || g != (len(c.ids) > 0) {
				t.Errorf("%s %+v: GrantedByScope %v %v", tc.name, c.res, g, err)
			}
			// Decide agrees for every single action.
			for _, act := range rwo {
				err := a.Decide(ctx, p, set(act), c.res)
				if allowed := slices.Contains(c.want, act); allowed != (err == nil) {
					t.Errorf("%s %+v %s: Decide %v, allowed %v", tc.name, c.res, act, err, allowed)
				}
			}
		}
	}
}

// TestDirectGrantLevelsInDecide: view and message cannot operate, control can;
// a denial with some action held is forbidden, with none not_found.
func TestDirectGrantLevelsInDecide(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	for level, want := range map[int]proto.Code{1: proto.CodeForbidden, 2: proto.CodeForbidden, 3: ""} {
		p := authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{ownerGrant("g", "W2", "x", level)})
		err := a.Decide(ctx, p, set(ops.ActionOperate), sess("W2", "x"))
		if want == "" {
			if err != nil {
				t.Errorf("level %d: %v", level, err)
			}
		} else if code(t, err) != want {
			t.Errorf("level %d: %v, want %s", level, err, want)
		}
		if ids, _ := a.GrantIDs(ctx, p, set(ops.ActionOperate), sess("W2", "x")); (len(ids) > 0) != (want == "") {
			t.Errorf("level %d: GrantIDs for operate %v", level, ids)
		}
		if code(t, a.Decide(ctx, p, set(ops.ActionRead), sess("W2", "y"))) != proto.CodeNotFound {
			t.Errorf("level %d: uncovered session is not not_found", level)
		}
		// manage is never granted.
		if a.Decide(ctx, p, set(ops.ActionManage), sess("W2", "x")) == nil {
			t.Errorf("level %d: manage allowed", level)
		}
	}
}

// TestDirectGrantLegacyScopeUnaffected: the session-control grant is still
// judged by the owner's base permission, next to a direct grant.
func TestDirectGrantLegacyScopeUnaffected(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	// alice's controller A1 holds a legacy grant on everything (reaches only
	// alice's Workers) and a direct view grant on bob's W3:build.
	p := authz.NewWorkerSessionPrincipal("A1", "main", grantScope(tEverything), []authz.DirectGrant{adminGrant("g", "W3", "build", 1)})
	for res, want := range map[authz.Resource][]ops.Action{
		sess("W2", "x"): rwo, sess("A2", "s"): rwo, sess("W3", "build"): ro, sess("W3", "other"): nil, sess("W1", "boss"): nil,
	} {
		got, err := a.EffectiveActions(ctx, p, res)
		if err != nil || !slices.Equal([]ops.Action(got), want) {
			t.Errorf("%+v: %v %v, want %v", res, got, err, want)
		}
	}
	// The legacy part is granted by scope but names no direct grant.
	if g, err := a.GrantedByScope(ctx, p, set(ops.ActionOperate), sess("W2", "x")); err != nil || !g {
		t.Errorf("legacy GrantedByScope %v %v", g, err)
	}
	if ids, err := a.GrantIDs(ctx, p, set(ops.ActionOperate), sess("W2", "x")); err != nil || len(ids) != 0 {
		t.Errorf("legacy GrantIDs %v %v", ids, err)
	}
}

// TestNoDirectGrantsDecidesAsToday: without direct grants the session
// principal decides exactly as NewWorkerGrantPrincipal, on the cases of
// grant_test.go.
func TestNoDirectGrantsDecidesAsToday(t *testing.T) {
	ctx := context.Background()
	resources := []authz.Resource{
		authz.WorkerResource{ID: "W1"}, sess("W1", "boss"), authz.WorkerResource{ID: "W2"}, sess("W2", "x"),
		authz.WorkerResource{ID: "W3"}, sess("W3", "build"), sess("W3", "other"),
		authz.WorkerResource{ID: "A1"}, sess("A1", "s"), authz.WorkerResource{ID: "A2"}, sess("A2", "s"),
		authz.WorkerResource{ID: "gone"}, sess("gone", "s"),
		authz.UserResource{ID: "admin"}, authz.UserResource{ID: "alice"}, authz.UserResource{ID: "bob"},
	}
	targetSets := [][]ops.KeyResource{
		nil, {tEverything}, {tW2}, {tW3Build}, {tA1}, {tW2, tW3Build}, {tW2, tA1},
	}
	mutations := map[string]func(*fakeStore){
		"as is":             func(*fakeStore) {},
		"admin demoted":     func(st *fakeStore) { st.setRole("admin", authz.RoleUser) },
		"alice deactivated": func(st *fakeStore) { st.users["alice"] = authz.UserState{Role: authz.RoleUser} },
		"W2 moved to bob":   func(st *fakeStore) { st.workers["W2"] = "bob" },
	}
	actionSets := []ops.ActionSet{
		set(ops.ActionRead), set(ops.ActionMessage), set(ops.ActionOperate), set(ops.ActionManage),
		set(ops.ActionRead, ops.ActionMessage, ops.ActionOperate),
	}
	errText := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	for name, mutate := range mutations {
		st := grantStore()
		mutate(st)
		a := dgAuthz(st)
		for _, controller := range []string{"W1", "A1", "W3", "gone"} {
			for _, targets := range targetSets {
				var sc authz.Scope
				if targets != nil {
					sc = grantScope(targets...)
				}
				old := authz.NewWorkerGrantPrincipal(controller, sc)
				for _, direct := range [][]authz.DirectGrant{nil, {}} {
					p := authz.NewWorkerSessionPrincipal(controller, "main", sc, direct)
					for _, res := range resources {
						at := name + " " + controller
						wantE, errW := a.EffectiveActions(ctx, old, res)
						gotE, errG := a.EffectiveActions(ctx, p, res)
						if !slices.Equal(gotE, wantE) || errText(errG) != errText(errW) {
							t.Errorf("%s %v %+v: actions %v %v, want %v %v", at, targets, res, gotE, errG, wantE, errW)
						}
						for _, acc := range actionSets {
							if g, w := errText(a.Decide(ctx, p, acc, res)), errText(a.Decide(ctx, old, acc, res)); g != w {
								t.Errorf("%s %v %+v %v: Decide %q, want %q", at, targets, res, acc, g, w)
							}
							wantS, _ := a.GrantedByScope(ctx, old, acc, res)
							gotS, _ := a.GrantedByScope(ctx, p, acc, res)
							if gotS != wantS {
								t.Errorf("%s %v %+v %v: GrantedByScope %v, want %v", at, targets, res, acc, gotS, wantS)
							}
							if ids, err := a.GrantIDs(ctx, p, acc, res); err != nil || ids != nil {
								t.Errorf("%s %v %+v: GrantIDs %v %v", at, targets, res, ids, err)
							}
						}
					}
					wantF, _ := authz.Filter(ctx, a, old, resources, func(r authz.Resource) authz.Resource { return r })
					gotF, _ := authz.Filter(ctx, a, p, resources, func(r authz.Resource) authz.Resource { return r })
					if !slices.Equal(gotF, wantF) {
						t.Errorf("%s %s %v: Filter %v, want %v", name, controller, targets, gotF, wantF)
					}
				}
			}
		}
	}
}

func TestDirectGrantFilter(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	p := authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{adminGrant("g", "W3", "build", 1)})
	workers := []string{"W1", "W2", "W3", "A1", "A2"}
	gotW, err := authz.Filter(ctx, a, p, workers, func(id string) authz.Resource { return authz.WorkerResource{ID: id} })
	if err != nil || !slices.Equal(gotW, []string{"W3", "A1"}) {
		t.Errorf("workers: %v %v", gotW, err)
	}
	type item struct{ worker, session string }
	items := []item{{"A1", "s"}, {"W2", "x"}, {"W3", "build"}, {"W3", "other"}, {"gone", "s"}}
	gotS, err := authz.Filter(ctx, a, p, items, func(i item) authz.Resource { return sess(i.worker, i.session) })
	if err != nil || !slices.Equal(gotS, []item{{"A1", "s"}, {"W3", "build"}}) {
		t.Errorf("sessions: %v %v", gotS, err)
	}
}

// grantorLookup fails the lookup of one user and counts the lookups per user.
type grantorLookup struct {
	*fakeStore
	failID string
	err    error
	count  map[string]int
}

func (l *grantorLookup) User(ctx context.Context, id string) (authz.UserState, error) {
	l.count[id]++
	if id == l.failID {
		return authz.UserState{}, l.err
	}
	return l.fakeStore.User(ctx, id)
}

func TestDirectGrantGrantorLookup(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("db down")
	st := grantStore()
	clock := authz.WithClock(func() time.Time { return dgNow })
	grants := []authz.DirectGrant{adminGrant("g1", "W3", "build", 3), adminGrant("g2", "W3", "other", 1)}
	p := authz.NewWorkerSessionPrincipal("A1", "main", nil, grants)

	// A failing grantor lookup denies and is returned.
	l := &grantorLookup{fakeStore: st, failID: "admin", err: boom, count: map[string]int{}}
	a := authz.New(l, st, clock)
	if err := a.Decide(ctx, p, set(ops.ActionRead), sess("W3", "build")); !errors.Is(err, boom) {
		t.Errorf("Decide err = %v, want the lookup error", err)
	}
	if got, err := a.EffectiveActions(ctx, p, sess("W3", "build")); !errors.Is(err, boom) || len(got) != 0 {
		t.Errorf("EffectiveActions = %v %v", got, err)
	}
	if g, err := a.GrantedByScope(ctx, p, set(ops.ActionRead), sess("W3", "build")); !errors.Is(err, boom) || g {
		t.Errorf("GrantedByScope = %v %v", g, err)
	}
	if ids, err := a.GrantIDs(ctx, p, set(ops.ActionRead), sess("W3", "build")); !errors.Is(err, boom) || ids != nil {
		t.Errorf("GrantIDs = %v %v", ids, err)
	}
	if got, err := authz.Filter(ctx, a, p, []string{"W3"}, func(id string) authz.Resource { return authz.WorkerResource{ID: id} }); !errors.Is(err, boom) || len(got) != 0 {
		t.Errorf("Filter = %v %v", got, err)
	}

	// The grantor is read once per decision, and again on the next one.
	l = &grantorLookup{fakeStore: st, count: map[string]int{}}
	a = authz.New(l, st, clock)
	for i := 1; i <= 2; i++ {
		if err := a.Decide(ctx, p, set(ops.ActionOperate), sess("W3", "build")); err != nil {
			t.Fatal(err)
		}
		if l.count["admin"] != i {
			t.Errorf("decision %d: %d grantor lookups", i, l.count["admin"])
		}
	}
	// A grant that cannot apply (suspended) costs no lookup.
	off := adminGrant("g", "W3", "build", 3)
	off.Suspended = true
	l.count = map[string]int{}
	_ = a.Decide(ctx, authz.NewWorkerSessionPrincipal("A1", "main", nil, []authz.DirectGrant{off}), set(ops.ActionRead), sess("W3", "build"))
	if l.count["admin"] != 0 {
		t.Errorf("suspended grant: %d grantor lookups", l.count["admin"])
	}
}
