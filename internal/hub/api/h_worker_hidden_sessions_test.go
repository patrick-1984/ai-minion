package api

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type fakeWorkerHiddenStore struct {
	owners map[string]string
	set    map[string]*bool
	// reassign, when set, is the owner the Worker has by the time the write
	// runs (a reassignment between the check and the write).
	reassign string
}

func (f *fakeWorkerHiddenStore) WorkerOwner(_ context.Context, id string) (string, error) {
	o, ok := f.owners[id]
	if !ok {
		return "", store.ErrWorkerNotFound
	}
	return o, nil
}

func (f *fakeWorkerHiddenStore) SetWorkerHiddenSessionsIfOwner(_ context.Context, id string, allow *bool, _, owner string) error {
	now, ok := f.owners[id]
	if !ok {
		return store.ErrWorkerNotFound
	}
	if f.reassign != "" {
		now = f.reassign
	}
	if owner != "" && now != owner {
		return store.ErrWorkerOwnerChanged
	}
	f.set[id] = allow
	return nil
}

// userManagerDecider grants manage to the users it lists (administrators).
type userManagerDecider struct{ admins map[string]bool }

func (d userManagerDecider) Decide(_ context.Context, p authz.Principal, accepted ops.ActionSet, _ authz.Resource) error {
	if accepted.Permits(ops.ActionManage) && d.admins[p.UserID] && p.Kind != authz.KindActAs {
		return nil
	}
	return proto.NewError(proto.CodeForbidden, "no")
}

// The owner and an administrator may set a Worker's choice; someone who
// holds operate through a grant, a Worker identity and an admin acting as
// another user (who is not the owner) may not.
func TestWorkerHiddenSessionsSetIsTheOwnersOrAnAdmins(t *testing.T) {
	wid := ops.NewUUIDv7()
	st := &fakeWorkerHiddenStore{owners: map[string]string{wid: "alice"}, set: map[string]*bool{}}
	h := workerHiddenHandlers{st: st, dec: userManagerDecider{admins: map[string]bool{"root": true}}, hub: func() string { return "user-controlled" }}
	args := ops.WorkerHiddenSessionsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WorkerID: wid, HiddenSessions: ops.WorkerHiddenAllow}
	for _, tc := range []struct {
		name string
		p    authz.Principal
		ok   bool
	}{
		{"owner", authz.NewSessionPrincipal("alice"), true},
		{"owner's key", authz.NewAPIKeyPrincipal("alice", allScope{}, ""), true},
		{"admin", authz.NewSessionPrincipal("root"), true},
		{"grant holder", authz.NewSessionPrincipal("carol"), false},
		{"admin acting as carol", authz.NewActAsPrincipal("root", "carol", time.Now().Add(time.Hour)), false},
		{"Worker", authz.NewWorkerPrincipal("w1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clear(st.set)
			c := &Call{Principal: tc.p, Identity: Identity{Actor: "actor"}}
			res, err := h.set(context.Background(), c, args)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok {
				if len(st.set) != 0 || ErrorOf(err).Code != proto.CodeForbidden {
					t.Errorf("refused call stored %v or returned %v", st.set, err)
				}
				return
			}
			if v := st.set[wid]; v == nil || !*v || c.AuditDetail != "hidden_sessions=allow" || len(res.Warnings) != 0 {
				t.Errorf("stored %v, audit detail %q, warnings %v", v, c.AuditDetail, res.Warnings)
			}
		})
	}
	// Unknown Worker: not found. A Hub policy that decides for everybody: a warning.
	c := &Call{Principal: authz.NewSessionPrincipal("root"), Identity: Identity{Actor: "root"}}
	unknown := args
	unknown.WorkerID = ops.NewUUIDv7()
	if _, err := h.set(context.Background(), c, unknown); ErrorOf(err).Code != proto.CodeNotFound {
		t.Errorf("unknown Worker: %v", err)
	}
	h.hub = func() string { return "disabled" }
	if res, err := h.set(context.Background(), c, args); err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != WarnHubPolicy {
		t.Errorf("under a disabled Hub: %+v, %v", res, err)
	}
}

// A choice decided for the owner is not stored once the Worker was reassigned
// before the write; an administrator's (manage) does not depend on the owner.
func TestWorkerHiddenSessionsSetRechecksTheOwnerAtTheWrite(t *testing.T) {
	wid := ops.NewUUIDv7()
	st := &fakeWorkerHiddenStore{owners: map[string]string{wid: "alice"}, set: map[string]*bool{}, reassign: "bob"}
	h := workerHiddenHandlers{st: st, dec: userManagerDecider{admins: map[string]bool{"root": true}}, hub: func() string { return "user-controlled" }}
	args := ops.WorkerHiddenSessionsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WorkerID: wid, HiddenSessions: ops.WorkerHiddenAllow}
	c := &Call{Principal: authz.NewSessionPrincipal("alice"), Identity: Identity{Actor: "alice"}}
	if _, err := h.set(context.Background(), c, args); ErrorOf(err).Code != proto.CodeConflict || len(st.set) != 0 {
		t.Fatalf("the old owner after a reassignment: %v, stored %v", err, st.set)
	}
	c = &Call{Principal: authz.NewSessionPrincipal("root"), Identity: Identity{Actor: "root"}}
	if _, err := h.set(context.Background(), c, args); err != nil || st.set[wid] == nil {
		t.Fatalf("an administrator: %v, stored %v", err, st.set)
	}
}
