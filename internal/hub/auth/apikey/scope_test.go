package apikey

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

var allFour = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}

// Fixed identifiers of the scope tables. u1 owns w1 and w2; u2 owns w3.
const (
	u1, u2     = "user-1", "user-2"
	w1, w2, w3 = "worker-1", "worker-2", "worker-3"
)

func userRes(id string) authz.ResolvedResource {
	return authz.ResolvedResource{Kind: authz.ResourceUser, UserID: id, OwnerID: id}
}

func workerRes(id, owner string) authz.ResolvedResource {
	return authz.ResolvedResource{Kind: authz.ResourceWorker, WorkerID: id, OwnerID: owner}
}

func sessionRes(w, name, owner string) authz.ResolvedResource {
	return authz.ResolvedResource{Kind: authz.ResourceSession, WorkerID: w, SessionName: name, OwnerID: owner}
}

// resources of the tables, with their owners.
var tableResources = []struct {
	name string
	res  authz.ResolvedResource
}{
	{"user u1", userRes(u1)},
	{"user u2", userRes(u2)},
	{"worker w1 (u1)", workerRes(w1, u1)},
	{"worker w2 (u1)", workerRes(w2, u1)},
	{"worker w3 (u2)", workerRes(w3, u2)},
	{"session w1/main", sessionRes(w1, "main", u1)},
	{"session w1/other", sessionRes(w1, "other", u1)},
	{"session w2/main", sessionRes(w2, "main", u1)},
	{"session w3/main", sessionRes(w3, "main", u2)},
}

func TestScopeCoverageTable(t *testing.T) {
	scopes := []struct {
		name   string
		grants []Grant
		// covered lists the table resources the scope reaches.
		covered []string
	}{
		{
			"everything", []Grant{{Kind: ops.KeyResourceEverything}},
			[]string{"user u1", "user u2", "worker w1 (u1)", "worker w2 (u1)", "worker w3 (u2)", "session w1/main", "session w1/other", "session w2/main", "session w3/main"},
		},
		{
			"chosen user u1", []Grant{{Kind: ops.KeyResourceUser, UserID: u1}},
			[]string{"user u1", "worker w1 (u1)", "worker w2 (u1)", "session w1/main", "session w1/other", "session w2/main"},
		},
		{
			"chosen users u1 and u2", []Grant{{Kind: ops.KeyResourceUser, UserID: u1}, {Kind: ops.KeyResourceUser, UserID: u2}},
			[]string{"user u1", "user u2", "worker w1 (u1)", "worker w2 (u1)", "worker w3 (u2)", "session w1/main", "session w1/other", "session w2/main", "session w3/main"},
		},
		{
			"chosen worker w1", []Grant{{Kind: ops.KeyResourceWorker, WorkerID: w1}},
			[]string{"worker w1 (u1)", "session w1/main", "session w1/other"},
		},
		{
			"chosen workers w1 and w3", []Grant{{Kind: ops.KeyResourceWorker, WorkerID: w1}, {Kind: ops.KeyResourceWorker, WorkerID: w3}},
			[]string{"worker w1 (u1)", "worker w3 (u2)", "session w1/main", "session w1/other", "session w3/main"},
		},
		{
			"chosen session w1/main", []Grant{{Kind: ops.KeyResourceSession, WorkerID: w1, SessionName: "main"}},
			[]string{"session w1/main"},
		},
		{
			"session name of another worker", []Grant{{Kind: ops.KeyResourceSession, WorkerID: w2, SessionName: "other"}},
			nil,
		},
		{
			"mixed worker and session", []Grant{{Kind: ops.KeyResourceWorker, WorkerID: w2}, {Kind: ops.KeyResourceSession, WorkerID: w3, SessionName: "main"}},
			[]string{"worker w2 (u1)", "session w2/main", "session w3/main"},
		},
		{"no grants", nil, nil},
	}
	for _, sc := range scopes {
		t.Run(sc.name, func(t *testing.T) {
			scope := NewScope(sc.grants, allFour)
			for _, r := range tableResources {
				got := scope.Actions(r.res)
				if slices.Contains(sc.covered, r.name) {
					want := []ops.Action{ops.ActionManage, ops.ActionMessage, ops.ActionOperate, ops.ActionRead}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s: actions = %v, want %v", r.name, got, want)
					}
				} else if got != nil {
					t.Errorf("%s: actions = %v, want none", r.name, got)
				}
			}
		})
	}
}

func TestScopeGrantsExactlyTheChosenActions(t *testing.T) {
	grants := []Grant{{Kind: ops.KeyResourceWorker, WorkerID: w1}}
	for _, actions := range [][]ops.Action{
		{ops.ActionRead},
		{ops.ActionMessage},
		{ops.ActionRead, ops.ActionMessage},
		{ops.ActionOperate},
		{ops.ActionManage},
	} {
		got := NewScope(grants, actions).Actions(workerRes(w1, u1))
		want := slices.Clone(actions)
		slices.Sort(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("actions %v: got %v", actions, got)
		}
		// message never stands in for operate and the reverse.
		if slices.Contains(actions, ops.ActionMessage) && !slices.Contains(actions, ops.ActionOperate) && slices.Contains(got, ops.ActionOperate) {
			t.Errorf("message key holds operate: %v", got)
		}
	}
}

func TestScopeIgnoresUnknownGrantsAndActions(t *testing.T) {
	s := NewScope([]Grant{
		{Kind: "team", UserID: u1},
		{Kind: ops.KeyResourceUser},                         // no id
		{Kind: ops.KeyResourceWorker},                       // no id
		{Kind: ops.KeyResourceSession, WorkerID: w1},        // no name
		{Kind: ops.KeyResourceSession, SessionName: "main"}, // no worker
	}, []ops.Action{"admin", ops.ActionRead, ops.ActionRead})
	for _, r := range tableResources {
		if got := s.Actions(r.res); got != nil {
			t.Errorf("%s: a malformed scope granted %v", r.name, got)
		}
	}
	// An unknown action is dropped and a duplicate collapses.
	got := NewScope([]Grant{{Kind: ops.KeyResourceEverything}}, []ops.Action{"admin", ops.ActionRead, ops.ActionRead}).Actions(userRes(u1))
	if !reflect.DeepEqual(got, []ops.Action{ops.ActionRead}) {
		t.Errorf("actions = %v", got)
	}
}

func TestScopeDoesNotMatchAnEmptyOwnerAndReturnsACopy(t *testing.T) {
	// A resource without an owner id must not match a (malformed) empty user.
	s := NewScope([]Grant{{Kind: ops.KeyResourceUser, UserID: ""}}, allFour)
	if got := s.Actions(workerRes(w1, "")); got != nil {
		t.Errorf("ownerless worker covered: %v", got)
	}
	s = NewScope([]Grant{{Kind: ops.KeyResourceEverything}}, []ops.Action{ops.ActionRead})
	a := s.Actions(userRes(u1))
	a[0] = ops.ActionManage
	if got := s.Actions(userRes(u1)); got[0] != ops.ActionRead {
		t.Errorf("the scope was changed through a returned slice: %v", got)
	}
}

// --- scope ∩ the owner's current permissions, through the real authorizer ---

type fixedUsers map[string]authz.UserState

func (f fixedUsers) User(_ context.Context, id string) (authz.UserState, error) {
	if u, ok := f[id]; ok {
		return u, nil
	}
	return authz.UserState{}, authz.ErrNotFound
}

type fixedWorkers map[string]string

func (f fixedWorkers) WorkerOwner(_ context.Context, id string) (string, error) {
	if o, ok := f[id]; ok {
		return o, nil
	}
	return "", authz.ErrNotFound
}

func TestEffectiveActionsAreScopeIntersectOwnerRole(t *testing.T) {
	users := fixedUsers{
		u1:      {Role: authz.RoleUser, Active: true},
		u2:      {Role: authz.RoleUser, Active: true},
		"admin": {Role: authz.RoleAdmin, Active: true},
	}
	workers := fixedWorkers{w1: u1, w2: u1, w3: u2}
	az := authz.New(users, workers)
	ctx := context.Background()

	rom := []ops.Action{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}
	tests := []struct {
		name   string
		owner  string
		grants []Grant
		res    authz.Resource
		want   []ops.Action
	}{
		// A plain user keeps read, operate and message on what they own, never manage.
		{"user, everything scope, own worker", u1, []Grant{{Kind: ops.KeyResourceEverything}}, authz.WorkerResource{ID: w1}, rom},
		{"user, everything scope, foreign worker", u1, []Grant{{Kind: ops.KeyResourceEverything}}, authz.WorkerResource{ID: w3}, nil},
		{"user, everything scope, foreign user", u1, []Grant{{Kind: ops.KeyResourceEverything}}, authz.UserResource{ID: u2}, nil},
		{"user, everything scope, own user", u1, []Grant{{Kind: ops.KeyResourceEverything}}, authz.UserResource{ID: u1}, rom},
		{"user, chosen user scope on a foreign user", u1, []Grant{{Kind: ops.KeyResourceUser, UserID: u2}}, authz.WorkerResource{ID: w3}, nil},
		{"user, chosen own worker", u1, []Grant{{Kind: ops.KeyResourceWorker, WorkerID: w1}}, authz.SessionResource{WorkerID: w1, Name: "main"}, rom},
		{"user, chosen own worker, other worker", u1, []Grant{{Kind: ops.KeyResourceWorker, WorkerID: w1}}, authz.WorkerResource{ID: w2}, nil},
		{"user, chosen session, same session", u1, []Grant{{Kind: ops.KeyResourceSession, WorkerID: w1, SessionName: "main"}}, authz.SessionResource{WorkerID: w1, Name: "main"}, rom},
		{"user, chosen session, other session", u1, []Grant{{Kind: ops.KeyResourceSession, WorkerID: w1, SessionName: "main"}}, authz.SessionResource{WorkerID: w1, Name: "other"}, nil},
		{"user, chosen session, the worker itself", u1, []Grant{{Kind: ops.KeyResourceSession, WorkerID: w1, SessionName: "main"}}, authz.WorkerResource{ID: w1}, nil},
		// An admin keeps everything the scope grants, manage included.
		{"admin, everything scope, foreign worker", "admin", []Grant{{Kind: ops.KeyResourceEverything}}, authz.WorkerResource{ID: w3}, allFourSorted()},
		{"admin, chosen user scope", "admin", []Grant{{Kind: ops.KeyResourceUser, UserID: u2}}, authz.SessionResource{WorkerID: w3, Name: "main"}, allFourSorted()},
		{"admin, chosen user scope, other user's worker", "admin", []Grant{{Kind: ops.KeyResourceUser, UserID: u2}}, authz.WorkerResource{ID: w1}, nil},
		{"admin, chosen worker", "admin", []Grant{{Kind: ops.KeyResourceWorker, WorkerID: w3}}, authz.WorkerResource{ID: w3}, allFourSorted()},
		{"admin, chosen session", "admin", []Grant{{Kind: ops.KeyResourceSession, WorkerID: w3, SessionName: "main"}}, authz.SessionResource{WorkerID: w3, Name: "main"}, allFourSorted()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := authz.NewAPIKeyPrincipal(tc.owner, NewScope(tc.grants, allFour), "")
			got, err := az.EffectiveActions(ctx, p, tc.res)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual([]ops.Action(got), tc.want) {
				t.Errorf("effective = %v, want %v", got, tc.want)
			}
		})
	}
}

func allFourSorted() []ops.Action {
	a := slices.Clone(allFour)
	slices.Sort(a)
	return a
}
