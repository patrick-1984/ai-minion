package workers

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// verifiedGrant stores a grant prepared -> build and binds it to incarnation.
func (r *masterRig) verifiedGrant(t *testing.T, incarnation string) store.SessionGrantRow {
	t.Helper()
	ctx := t.Context()
	if _, err := r.master.db.DB().ExecContext(ctx, `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	g, err := r.master.db.PutSessionGrant(ctx, store.SessionGrantRow{SourceWorkerID: r.source.ID, SourceSession: "prepared",
		TargetWorkerID: r.target.ID, TargetSession: "build", Level: 2, Authority: store.GrantAuthorityOwner, GrantedBy: "owner",
		SourceOwnerID: "owner", TargetOwnerID: "owner"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if g, err = r.master.db.BindSessionGrant(ctx, g.ID, incarnation, g.Revision); err != nil {
		t.Fatal(err)
	}
	r.master.RefreshLeases(r.source.ID)
	return g
}

func TestVerifiedGrantGetsALeaseAndIsServedOnlyForItsIncarnation(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	// End the nomination of the rig: the terminal is a candidate only.
	gen := r.lane.leases["first"].binding.Generation
	if _, err := m.SetBoundMaster(ctx, r.source, r.change(gen, false), "owner"); err != nil {
		t.Fatal(err)
	}
	if len(r.lane.leases) != 0 {
		t.Fatalf("a candidate without nomination or grant holds a lease: %+v", r.lane.leases)
	}
	if inc, ok := m.PreparedIncarnation(ctx, r.source.ID, "prepared"); !ok || inc != "first" {
		t.Fatalf("prepared incarnation = %q, %v", inc, ok)
	}
	if _, ok := m.PreparedIncarnation(ctx, r.source.ID, "other"); ok {
		t.Fatal("a session that is not reported is a prepared terminal")
	}

	// A grant bound to another incarnation gives this terminal no lease.
	other := r.verifiedGrant(t, "second")
	if len(r.lane.leases) != 0 {
		t.Fatalf("a grant of another incarnation gave a lease: %+v", r.lane.leases)
	}
	forged := Call{Worker: r.source, TranscriptHash: r.lane.info.TranscriptHash, RequestID: ops.NewUUIDv7(),
		Master: &proto.MasterCallArgs{Generation: grantLeaseGeneration, IncarnationID: "first", LeaseID: ops.NewUUIDv7(),
			Operation: ops.OpWorkersList, Arguments: []byte(`{}`)}}
	if _, err := m.runMasterCall(ctx, forged); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("another incarnation was served: %v", err)
	}
	if _, err := m.db.DeleteSessionGrant(ctx, other.ID); err != nil {
		t.Fatal(err)
	}

	// Bound to the reported incarnation: a lease that stands on the grant.
	g := r.verifiedGrant(t, "first")
	lease := r.lane.leases["first"]
	if lease.master || lease.binding.LeaseID == "" || lease.row.SessionName != "prepared" {
		t.Fatalf("grant lease = %+v", lease)
	}
	out, err := m.runMasterCall(ctx, r.call(ops.OpWorkersList))
	if err != nil {
		t.Fatal(err)
	}
	if ws := out.(ops.WorkersListResult).Workers; len(ws) != 1 || ws[0].ID != r.target.ID {
		t.Fatalf("workers_list under a verified grant = %+v", ws)
	}
	auth, l, err := m.masterAuthority(ctx, r.call(ops.OpWorkersList))
	if err != nil || l != r.lane || auth.scope != nil || len(auth.direct) != 1 || !auth.reach[r.target.ID]["build"] {
		t.Fatalf("authority = %+v, %v", auth, err)
	}
	if p := auth.principal(r.source.ID); p.SessionName() != "prepared" || len(p.DirectGrants()) != 1 || p.Scope != nil {
		t.Fatalf("principal = %+v", p)
	}
	// The final fence admits the granted session and refuses another one,
	// the own Worker and a grant that changed after the decision.
	c := r.call(ops.OpSessionSnapshot)
	if err := m.admitMaster(ctx, c, &masterPending{}, r.backend.on, "build", auth); err != nil {
		t.Fatalf("final fence refused the granted session: %v", err)
	}
	if err := m.admitMaster(ctx, c, &masterPending{}, r.backend.on, "elsewhere", auth); err == nil {
		t.Fatal("final fence admitted a session without grant")
	}
	stale := auth
	stale.key = "#direct:old"
	if err := m.admitMaster(ctx, c, &masterPending{}, r.backend.on, "build", stale); err == nil {
		t.Fatal("final fence admitted grants that changed")
	}
	// The name-only path cannot use the verified grant.
	if rows, err := m.db.SessionGrantsFor(ctx, r.source.ID, "prepared"); err != nil || len(rows) != 0 {
		t.Fatalf("name-only grants = %+v, %v", rows, err)
	}

	// Revoke: the next call is refused and the lease ends with it.
	if _, err := m.db.DeleteSessionGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.runMasterCall(ctx, c); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("a revoked grant was served: %v", err)
	}
	if len(r.lane.leases) != 0 {
		t.Fatalf("the lease outlived the revoke: %+v", r.lane.leases)
	}
}

func TestMasterWithVerifiedGrantKeepsBothAuthorities(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	before := r.lane.leases["first"].binding
	r.verifiedGrant(t, "first")
	if lease := r.lane.leases["first"]; !lease.master || lease.binding != before {
		t.Fatalf("the nomination lease changed: %+v", lease)
	}
	auth, _, err := m.masterAuthority(ctx, r.call(ops.OpWorkersList))
	if err != nil || auth.scope == nil || len(auth.direct) != 1 || !auth.reach[r.target.ID]["build"] {
		t.Fatalf("authority = %+v, %v", auth, err)
	}
	// The nomination reaches nothing (no accept): the grant admits.
	if err := m.admitMaster(ctx, r.call(ops.OpSessionSnapshot), &masterPending{}, r.backend.on, "build", auth); err != nil {
		t.Fatalf("final fence = %v", err)
	}
}
