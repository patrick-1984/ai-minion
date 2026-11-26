package workers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// verifiedGrantLevel stores a grant prepared -> build at level and binds it
// to incarnation.
func (r *masterRig) verifiedGrantLevel(t *testing.T, incarnation string, level int) store.SessionGrantRow {
	t.Helper()
	ctx := t.Context()
	if _, err := r.master.db.DB().ExecContext(ctx, `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	g, err := r.master.db.PutSessionGrant(ctx, store.SessionGrantRow{SourceWorkerID: r.source.ID, SourceSession: "prepared",
		TargetWorkerID: r.target.ID, TargetSession: "build", Level: level, Authority: store.GrantAuthorityOwner, GrantedBy: "owner",
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

// withUsers gives the Router of the rig an authorizer on users (a map the
// test may change while no decision runs).
func (r *masterRig) withUsers(t *testing.T, users fakeUsers) {
	t.Helper()
	dir := r.master.router.dir.(*fakeDir)
	r.master.router.az = authz.New(users, dir)
}

func (r *masterRig) prompt(t *testing.T) Call {
	t.Helper()
	c := r.call(ops.OpSendText)
	c.Master.Arguments, _ = json.Marshal(ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: c.RequestID},
		Target: ops.Target{WorkerID: r.target.ID, SessionName: "build"}, Text: "Run the tests", Submit: true})
	return c
}

// runBlocked runs c; change runs while the call waits for its destination
// stream (after the decision, before the final admission).
func (r *masterRig) runBlocked(t *testing.T, ctx context.Context, c Call, change func()) (any, error) {
	t.Helper()
	type answer struct {
		v   any
		err error
	}
	done := make(chan answer, 1)
	go func() {
		v, err := r.master.runMasterCall(ctx, c)
		done <- answer{v, err}
	}()
	select {
	case <-r.backend.opened:
	case a := <-done:
		t.Fatalf("the call ended before its dispatch: %+v, %v", a.v, a.err)
	case <-ctx.Done():
		t.Fatal("the call was not dispatched")
	}
	change()
	close(r.backend.goOn)
	close(r.stream.release)
	a := <-done
	return a.v, a.err
}

func (r *masterRig) wrote() bool {
	select {
	case <-r.stream.sent:
		return true
	default:
		return false
	}
}

// Finding: a Verified View grant admitted a prompt prepared under Control.
// The final admission decides the requested action again: once the Control
// that the legacy acceptance gave is withdrawn, the View grant that remains
// does not admit the prompt.
func TestVerifiedViewGrantDoesNotAdmitAPromptAfterControlIsWithdrawn(t *testing.T) {
	r := newMasterRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	if err := r.master.db.PutAccept(ctx, store.AcceptRow{TargetWorkerID: r.target.ID, SourceWorkerID: r.source.ID, Sessions: []string{"build"}}); err != nil {
		t.Fatal(err)
	}
	r.verifiedGrantLevel(t, "first", authz.LevelView)
	r.backend.on.Features = []string{proto.FeatureSendTextAgentOnly}
	_, err := r.runBlocked(t, ctx, r.prompt(t), func() {
		if _, err := r.master.db.DeleteAccept(ctx, r.target.ID, r.source.ID); err != nil {
			t.Error(err)
		}
	})
	if err == nil || r.wrote() {
		t.Fatalf("a View grant admitted a prompt after Control was withdrawn: %v, wrote=%v", err, r.wrote())
	}
}

// Finding: an owner grant dispatched after the owner was deactivated. The
// final admission decides again on a freshly built principal, which reads
// the owner of the calling Worker now.
func TestVerifiedGrantDoesNotDispatchAfterTheOwnerIsDeactivated(t *testing.T) {
	r := newMasterRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	users := fakeUsers{"owner": {Active: true, Role: authz.RoleUser}}
	r.withUsers(t, users)
	r.endNomination(t)
	r.verifiedGrantLevel(t, "first", authz.LevelControl)
	_, err := r.runBlocked(t, ctx, r.prompt(t), func() { users["owner"] = authz.UserState{Active: false, Role: authz.RoleUser} })
	if err == nil || r.wrote() {
		t.Fatalf("a grant of a deactivated owner was dispatched: %v, wrote=%v", err, r.wrote())
	}
}

// The same on the Claimed path: the call server's fence rebuilds the
// principal and the Router decides on it before it writes.
func TestClaimedGrantDoesNotDispatchAfterTheOwnerIsDeactivated(t *testing.T) {
	users := fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}, idBob: {Role: authz.RoleUser, Active: true},
		idRoot: {Role: authz.RoleAdmin, Active: true}}
	r := newSCRig(t)
	r.rt.az = authz.New(users, r.dir)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl))
	r.be.beforeOpen = func(*fakeBackend) { users[idAlice] = authz.UserState{Role: authz.RoleUser, Active: false} }
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden && c != proto.CodeNotFound {
		t.Fatalf("dispatch after the owner was deactivated: %q", c)
	}
	sent.mu.Lock()
	defer sent.mu.Unlock()
	if len(sent.written) != 0 {
		t.Fatalf("%d frames reached the Worker", len(sent.written))
	}
}

// Finding: a Claimed grant downgraded to View between the decision and the
// dispatch of a Control write. The fence refuses (the grant changed) and the
// decision on the new principal would refuse too.
func TestClaimedFinalAdmissionDecidesTheRequestedAction(t *testing.T) {
	r := newSCRig(t)
	g := aliceGrant(authz.LevelControl)
	cs := r.cs
	ctx := context.Background()
	r.grants.setDirect(wAlicePi1.ID, "agent", g)
	_, _, key, err := cs.authenticate(ctx, wAlicePi1.ID, "", "agent", ops.OpSendKeys)
	if err != nil {
		t.Fatal(err)
	}
	admit := cs.admitGrant(wAlicePi1.ID, "", "agent", ops.OpSendKeys, key)
	c := &call{admit: admit, accepted: ops.MustActionSet(ops.ActionOperate),
		res: authz.SessionResource{WorkerID: wAlicePi2.ID, Name: "build"}}
	if err := r.rt.admit(ctx, c, Online{}); err != nil {
		t.Fatalf("an unchanged Control grant: %v", err)
	}
	// The principal the fence builds is decided for the action: a View
	// grant read now (with the key it was authorized with) is refused.
	view := aliceGrant(authz.LevelView)
	r.grants.setDirect(wAlicePi1.ID, "agent", view)
	_, _, viewKey, _ := cs.authenticate(ctx, wAlicePi1.ID, "", "agent", ops.OpSendKeys)
	c.admit = cs.admitGrant(wAlicePi1.ID, "", "agent", ops.OpSendKeys, viewKey)
	if err := r.rt.admit(ctx, c, Online{}); code(err) != proto.CodeForbidden {
		t.Fatalf("a View grant admitted operate: %v", err)
	}
}

// Finding: a removed session's grant reached its replacement on the Verified
// path. The named target must still be the terminal the grant was made for,
// whatever time has passed.
func TestVerifiedGrantDoesNotReachTheReplacementOfItsTarget(t *testing.T) {
	r := newMasterRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	r.endNomination(t)
	r.verifiedGrantLevel(t, "first", authz.LevelView)
	snap := r.call(ops.OpSessionSnapshot)
	snap.Master.Arguments, _ = json.Marshal(ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: r.target.ID, SessionName: "build"}})
	if _, _, err := r.master.masterAuthority(ctx, snap); err != nil {
		t.Fatalf("the grant does not stand: %v", err)
	}
	// The target terminal ends and a new one takes its name.
	f, err := r.master.events.Connect(r.target.ID, r.target.Name).NewFeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SnapshotShare(0, nil, &ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // the new terminal is first seen after the grant was made
	if err := f.Apply(Change{Seq: 1, Event: EventSessionAdded, Session: SessionState{Name: "build", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	r.master.RefreshLeases(r.source.ID)
	close(r.backend.goOn)
	close(r.stream.release)
	if _, err := r.master.runMasterCall(ctx, snap); err == nil {
		t.Fatal("a grant made for the closed terminal reached its replacement")
	}
	select {
	case <-r.backend.opened:
		t.Fatal("a fenced grant reached the target Worker")
	default:
	}
}

// Finding: Verified prompts were missing from grant history. The write audit
// names the grant that admitted it and its use is counted.
func TestVerifiedPromptNamesItsGrantAndCountsItsUse(t *testing.T) {
	r := newMasterRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	r.master.auditor = audit.New(r.master.db)
	use := NewGrantUse(nil)
	r.master.SetGrantUse(use)
	r.endNomination(t)
	g := r.verifiedGrantLevel(t, "first", authz.LevelMessage)
	r.backend.on.Features = []string{proto.FeatureSendTextAgentOnly}
	close(r.backend.goOn)
	close(r.stream.release)
	out, err := r.master.runMasterCall(ctx, r.prompt(t))
	if w, ok := out.(ops.WriteResult); err != nil || !ok || w.Outcome != ops.OutcomeDelivered {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	rows, err := r.master.db.ListAudit(ctx, store.AuditQuery{GrantID: g.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		found = found || row.Action == ops.OpSendText
	}
	if !found {
		t.Fatalf("the Verified prompt is missing from the history of grant %s: %+v", g.ID, rows)
	}
	use.mu.Lock()
	n := use.pending[g.ID]
	use.mu.Unlock()
	if n != 1 {
		t.Fatalf("uses of the grant = %d, want 1", n)
	}
}

// Finding: a running Verified read outlived a grant change that no longer
// gives read while the target still looks reached. The re-check decides the
// read again on the principal built from the changed grants.
func TestVerifiedReadEndsWhenAChangedGrantNoLongerGivesRead(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	m.readRecheck = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	r.endNomination(t)
	g := r.verifiedGrantLevel(t, "first", authz.LevelView)
	stalled := newFakeStream()
	stalled.hang = true
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
	}
	select {
	case err := <-done:
		t.Fatalf("an authorized read was withdrawn: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	// A new revision whose authority no longer stands (an administrator
	// grant whose grantor is no administrator): the grant still names the
	// target (reach), but it no longer gives read.
	if _, err := m.db.DB().ExecContext(ctx, `UPDATE session_grant SET authority='admin', revision=revision+1 WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errMasterDenied) || !stalled.wasReset() {
			t.Fatalf("ended with %v, reset=%v", err, stalled.wasReset())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a read outlived a grant change that no longer gives read")
	}
}
