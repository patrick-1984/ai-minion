package api

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type fakePrefsStore struct {
	set    map[string]bool
	wanted map[string][]string
}

func (f *fakePrefsStore) SetUserPrefs(_ context.Context, id string, wanted *[]string, allow *bool) error {
	if wanted != nil {
		if f.wanted == nil {
			f.wanted = map[string][]string{}
		}
		f.wanted[id] = *wanted
	}
	if allow != nil {
		f.set[id] = *allow
	}
	return nil
}

func (f *fakePrefsStore) UserPrefs(_ context.Context, id string) (store.UserPrefs, error) {
	return store.UserPrefs{AllowHiddenSessions: f.set[id], WantedHarnesses: f.wanted[id]}, nil
}

// managerDecider grants manage to the principals it lists.
type managerDecider struct{ managers map[string]bool }

func (d managerDecider) Decide(_ context.Context, p authz.Principal, accepted ops.ActionSet, _ authz.Resource) error {
	if accepted.Permits(ops.ActionManage) && d.managers[p.KeyID] {
		return nil
	}
	return proto.NewError(proto.CodeForbidden, "no")
}

// The account-wide opt-in to hidden sessions is changed only from the
// person's own browser session or with a key that holds manage; an existing
// key with operate and an admin acting as the user cannot change it.
func TestAccountPrefsSetNeedsASessionOrManage(t *testing.T) {
	st := &fakePrefsStore{set: map[string]bool{}}
	h := &accountPrefsHandlers{st: st, dec: managerDecider{managers: map[string]bool{"admin-key": true}},
		hub: func() string { return "user-controlled" }, allows: func(_ string, u bool) bool { return u }}
	on := true
	args := ops.AccountPrefsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, AllowHiddenSessions: &on}
	for _, tc := range []struct {
		name string
		c    Call
		ok   bool
	}{
		{"own browser session", Call{Principal: authz.NewSessionPrincipal("alice"), Identity: Identity{Session: &session.Session{}}}, true},
		{"session principal without a session", Call{Principal: authz.NewSessionPrincipal("alice")}, false},
		{"operate key", Call{Principal: authz.NewAPIKeyPrincipal("alice", allScope{}, "").WithKeyID("alice-key")}, false},
		{"manage key", Call{Principal: authz.NewAPIKeyPrincipal("root", allScope{}, "").WithKeyID("admin-key")}, true},
		{"act-as", Call{Principal: authz.NewActAsPrincipal("root", "alice", time.Now().Add(time.Hour)), Identity: Identity{Session: &session.Session{}}}, false},
		{"Worker", Call{Principal: authz.NewWorkerPrincipal("w1")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clear(st.set)
			_, err := h.set(context.Background(), &tc.c, args)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok && (len(st.set) != 0 || ErrorOf(err).Code != proto.CodeForbidden) {
				t.Errorf("refused call stored %v or returned %v", st.set, err)
			}
		})
	}
}

// The harness choice is stored on its own, leaves the hidden-sessions opt-in
// alone, and a malformed name is refused before any store call.
func TestAccountPrefsSetWantedHarnesses(t *testing.T) {
	st := &fakePrefsStore{set: map[string]bool{}}
	h := &accountPrefsHandlers{st: st, dec: managerDecider{}, hub: func() string { return "user-controlled" }, allows: func(_ string, u bool) bool { return u }}
	c := &Call{Principal: authz.NewSessionPrincipal("alice"), Identity: Identity{Session: &session.Session{}}}
	names := []string{"claude", "codex"}
	args := ops.AccountPrefsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WantedHarnesses: &names}
	if _, err := h.set(context.Background(), c, args); err != nil {
		t.Fatal(err)
	}
	if got := st.wanted["alice"]; len(got) != 2 || got[1] != "codex" || len(st.set) != 0 {
		t.Fatalf("stored %v / %v", got, st.set)
	}
	res, err := h.get(context.Background(), c, ops.AccountPrefsArgs{})
	if err != nil || len(res.WantedHarnesses) != 2 {
		t.Fatalf("get = %+v, %v", res, err)
	}
	bad := []string{"Claude Code"}
	args.WantedHarnesses = &bad
	if err := args.Validate(); err == nil {
		t.Fatal("an invalid harness name was accepted")
	}
}
