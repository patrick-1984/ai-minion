package workers

import (
	"context"
	"encoding/json"
	"errors"
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
		SourceOwnerID: "owner", TargetOwnerID: "owner"}, 0, store.GrantWrite{ActorID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if g, err = r.master.db.BindSessionGrant(ctx, g.ID, incarnation, g.Revision, store.GrantWrite{ActorID: "owner"}); err != nil {
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
	if _, err := m.admitMaster(ctx, c, &masterPending{}, r.backend.on, "build", auth); err != nil {
		t.Fatalf("final fence refused the granted session: %v", err)
	}
	if _, err := m.admitMaster(ctx, c, &masterPending{}, r.backend.on, "elsewhere", auth); err == nil {
		t.Fatal("final fence admitted a session without grant")
	}
	stale := auth
	stale.key = "#direct:old"
	if _, err := m.admitMaster(ctx, c, &masterPending{}, r.backend.on, "build", stale); err == nil {
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
	if _, err := m.admitMaster(ctx, r.call(ops.OpSessionSnapshot), &masterPending{}, r.backend.on, "build", auth); err != nil {
		t.Fatalf("final fence = %v", err)
	}
}

// endNomination leaves the rig's terminal a candidate whose only authority is
// what verified grants give it.
func (r *masterRig) endNomination(t *testing.T) {
	t.Helper()
	gen := r.lane.leases["first"].binding.Generation
	if _, err := r.master.SetBoundMaster(t.Context(), r.source, r.change(gen, false), "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedMessageGrantSendsARoutedPrompt(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	r.endNomination(t)
	g := r.verifiedGrant(t, "first") // Message: read and message, no operate
	r.backend.on.Features = []string{proto.FeatureSendTextAgentOnly}
	close(r.backend.goOn)
	close(r.stream.release)

	c := r.call(ops.OpSendText)
	c.Master.Arguments, _ = json.Marshal(ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: c.RequestID},
		Target: ops.Target{WorkerID: r.target.ID, SessionName: "build"}, Text: "Run the tests", Submit: true})
	out, err := m.runMasterCall(ctx, c)
	if w, ok := out.(ops.WriteResult); err != nil || !ok || w.Outcome != ops.OutcomeDelivered {
		t.Fatalf("a prompt under a verified Message grant = %+v, %v", out, err)
	}
	request := r.stream.request(t)
	var args ops.SendTextArgs
	if err := json.Unmarshal(request.Args, &args); err != nil || !args.AgentOnly || !args.Submit || args.Force ||
		request.RequestID != c.RequestID {
		t.Fatalf("the routed prompt lost its guard: %+v, %+v", request, args)
	}
	// The target audits the session of the bound lease as verified, and the
	// grant of the final admission (review r1).
	if o := request.Origin; o.SourceSession != "prepared" || o.SourceProof != proto.OriginProofVerified || o.GrantID != g.ID ||
		request.TargetWorkerID != r.target.ID {
		t.Fatalf("the routed prompt lost its attribution: %+v, target %q", o, request.TargetWorkerID)
	}
}

func TestVerifiedReadEndsWhenItsGrantIsRevokedWhileTheTargetStalls(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	m.readRecheck = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	r.endNomination(t)
	g := r.verifiedGrant(t, "first")
	stalled := newFakeStream()
	stalled.hang = true // the target never answers
	r.backend.stream = stalled
	close(r.backend.goOn)

	c := r.call(ops.OpSessionSnapshot)
	c.Master.Arguments, _ = json.Marshal(ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: r.target.ID, SessionName: "build"}})
	done := make(chan error, 1)
	go func() {
		_, err := m.runMasterCall(ctx, c)
		done <- err
	}()
	select {
	case <-r.backend.opened:
	case err := <-done:
		t.Fatalf("the read ended before its dispatch: %v", err)
	case <-ctx.Done():
		t.Fatal("the read was not dispatched")
	}
	// Admitted and waiting: several re-checks pass while the grant stands.
	select {
	case err := <-done:
		t.Fatalf("an authorized read was withdrawn: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := m.db.DeleteSessionGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errMasterDenied) || !stalled.wasReset() {
			t.Fatalf("revoked read ended with %v, reset=%v", err, stalled.wasReset())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an admitted read outlived the revoke of its only grant")
	}
}
