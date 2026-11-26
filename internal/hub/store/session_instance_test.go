package store

import (
	"errors"
	"testing"
)

// The last instance of a session name is stored per Worker, replaced by a
// newer one, never empty, and goes with its Worker.
func TestSessionInstancesRoundTrip(t *testing.T) {
	f := newGrantFixture(t)
	s, ctx := f.s, t.Context()
	if got, err := s.LoadSessionInstances(ctx, f.a.ID); err != nil || len(got) != 0 {
		t.Fatalf("empty store = %+v, %v", got, err)
	}
	if err := s.SaveSessionInstances(ctx, f.a.ID, map[string]string{"boss": "i1", "x": "", "": "i9"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSessionInstances(ctx, f.b.ID, map[string]string{"boss": "j1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSessionInstances(ctx, f.a.ID, map[string]string{"boss": "i2", "ci": "c1"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSessionInstances(ctx, f.a.ID)
	if err != nil || len(got) != 2 || got["boss"] != "i2" || got["ci"] != "c1" {
		t.Fatalf("instances of a = %+v, %v", got, err)
	}
	if err := s.DeleteSessionInstances(ctx, f.a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadSessionInstances(ctx, f.a.ID); len(got) != 0 {
		t.Fatalf("after delete = %+v", got)
	}
	// A removed Worker's rows go with it (trigger of 0037).
	f.exec(t, `DELETE FROM workers WHERE id=?`, f.b.ID)
	if got, _ := s.LoadSessionInstances(ctx, f.b.ID); len(got) != 0 {
		t.Fatalf("instances of a removed Worker = %+v", got)
	}
}

// Reassigning a Worker to another owner ends every session grant that
// touches it, as source and as target, in the reassignment transaction, with
// one audit row each (cause owner_changed), like its master and acceptance
// rows (docs/guides/access.md, "A Worker changes owner").
func TestReassignWorkerEndsItsSessionGrants(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	f.exec(t, `UPDATE workers SET kind=? WHERE id=?`, WorkerKindInfrastructure, f.b.ID)
	asTarget := f.put(t, f.ownerGrant("boss")) // a -> b
	asSource := f.put(t, SessionGrantRow{SourceWorkerID: f.b.ID, SourceSession: "ci", TargetWorkerID: f.a.ID, Level: 1,
		Authority: GrantAuthorityOwner, GrantedBy: "alice", SourceOwnerID: "alice", TargetOwnerID: "alice", UpdatedAt: 100})
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: f.b.ID, NewOwnerID: "bob", AtMS: 5000}); err != nil {
		t.Fatal(err)
	}
	for _, g := range []SessionGrantRow{asTarget, asSource} {
		if _, err := f.s.SessionGrant(ctx, g.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("grant %s survived the reassignment: %v", g.ID, err)
		}
		wantSystemRow(t, f.s, g, AuditActionGrantEnd, "cause=owner_changed")
	}
}
