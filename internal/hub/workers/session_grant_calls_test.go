package workers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// aliceGrant is an owner grant from alice's pi-1 to session "build" on her
// pi-2 at the level.
func aliceGrant(level int) store.SessionGrantRow {
	return store.SessionGrantRow{
		ID: "g-1", SourceWorkerID: wAlicePi1.ID, SourceSession: "agent", TargetWorkerID: wAlicePi2.ID, TargetSession: "build",
		Level: level, Authority: store.GrantAuthorityOwner, GrantedBy: idAlice, SourceOwnerID: idAlice, TargetOwnerID: idAlice,
		Revision: 1,
	}
}

var grantedBuild = ops.Target{WorkerID: wAlicePi2.ID, SessionName: "build"}

// A View grant reads the granted session and nothing else: send_text is
// refused, another session of the target Worker is refused, and nothing of
// that reaches a Worker.
func TestSessionGrantViewReadsOnly(t *testing.T) {
	r := newSCRig(t)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))

	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionSnapshot,
		scArgs(ops.OpSessionSnapshot, grantedBuild), "", "agent"), map[string]any{})
	var snap ops.SessionSnapshotResult
	resultOf(t, frames, &snap)
	if o := sent.request(t).Origin; !o.Grant || o.SourceWorkerID != wAlicePi1.ID {
		t.Fatalf("origin = %+v", o)
	}

	opens := r.be.openCount()
	frames, _ = r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("send_text with a View grant: %q, want forbidden", c)
	}
	// Another Worker of another owner is out of reach whatever the op.
	frames, _ = r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionSnapshot,
		scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wBobPi1.ID, SessionName: "build"}), "", "agent"), map[string]any{})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden && c != proto.CodeNotFound {
		t.Fatalf("snapshot of an ungranted Worker: %q, want a refusal", c)
	}
	if r.be.openCount() != opens {
		t.Fatal("a refused request reached a Worker")
	}
}

// A Control grant forwards send_text, marked as a grant decision.
func TestSessionGrantControlSendsText(t *testing.T) {
	r := newSCRig(t)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl))
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	if wr.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text = %+v", wr)
	}
	if req := sent.request(t); req.Op != ops.OpSendText || !req.Origin.Grant || req.Origin.Kind != proto.OriginWorker {
		t.Fatalf("request = %+v", req)
	}
}

// Another session of the same Worker has no grant and is refused as before;
// a revoked grant is refused on the next call; a suspended one gives nothing.
func TestSessionGrantMissingRevokedSuspended(t *testing.T) {
	r := newSCRig(t)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl))
	args := scArgs(ops.OpSessionSnapshot, grantedBuild)
	opens := r.be.openCount()

	frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionSnapshot, args, "", "other"), map[string]any{})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("session without a grant: %q, want forbidden", c)
	}

	sus := aliceGrant(authz.LevelControl)
	at := int64(1)
	sus.SuspendedAt = &at
	r.grants.setDirect(wAlicePi1.ID, "agent", sus)
	frames, _ = r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"), map[string]any{})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden && c != proto.CodeNotFound {
		t.Fatalf("suspended grant: %q, want a refusal", c)
	}

	r.grants.setDirect(wAlicePi1.ID, "agent")
	frames, _ = r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionSnapshot, args, "", "agent"), map[string]any{})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("after revoke: %q, want forbidden", c)
	}
	if r.be.openCount() != opens {
		t.Fatal("a refused request reached a Worker")
	}
}

// sessions_list of the target Worker shows only the granted session.
func TestSessionGrantSessionsListShowsOnlyTheGrantedSession(t *testing.T) {
	r := newSCRig(t)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))
	one := func(name string) map[string]any {
		return map[string]any{"name": name, "created": "2026-11-09T12:00:00.000Z", "windows": 1, "attached": false, "pane": nil}
	}
	frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionsList, map[string]any{"worker_id": wAlicePi2.ID}, "", "agent"),
		map[string]any{"sessions": []any{one("build"), one("secret")}})
	var sl ops.SessionsListResult
	resultOf(t, frames, &sl)
	if len(sl.Sessions) != 1 || sl.Sessions[0].Name != "build" {
		t.Fatalf("sessions_list = %+v", sl.Sessions)
	}
}

// The value the 30 s re-check compares changes with the direct rows, and is
// the stored targets alone for a session without direct grants.
func TestSessionGrantRecheckKeyFollowsTheDirectRows(t *testing.T) {
	g := &fakeGrants{}
	g.set(t, wRootLab.ID, "boss", gAlicePi2)
	cs := NewCallServer(CallServerConfig{Grants: g, Logger: slog.New(slog.DiscardHandler)})
	ctx := context.Background()
	legacy, err := cs.lookupGrant(ctx, wRootLab.ID, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, key, err := cs.authenticate(ctx, wRootLab.ID, "", "boss", ops.OpSendText); err != nil || key != legacy {
		t.Fatalf("without direct rows: key %q, want the stored targets %q (%v)", key, legacy, err)
	}
	row := aliceGrant(authz.LevelView)
	g.setDirect(wRootLab.ID, "boss", row)
	pr, _, k1, err := cs.authenticate(ctx, wRootLab.ID, "", "boss", ops.OpSendText)
	if err != nil || k1 == legacy || pr.SessionName() != "boss" || len(pr.DirectGrants()) != 1 || pr.Scope == nil {
		t.Fatalf("with a direct row: key %q principal %+v (%v)", k1, pr, err)
	}
	if cur, _ := cs.grantKey(ctx, wRootLab.ID, "boss", true); cur != k1 {
		t.Fatalf("unchanged rows: %q != %q", cur, k1)
	}
	seen := map[string]bool{k1: true}
	exp, at := int64(5000), int64(7)
	for name, change := range map[string]func(*store.SessionGrantRow){
		"revision": func(r *store.SessionGrantRow) { r.Revision = 2 },
		"expiry":   func(r *store.SessionGrantRow) { r.ExpiresAt = &exp },
		"suspend":  func(r *store.SessionGrantRow) { r.SuspendedAt = &at },
		"id":       func(r *store.SessionGrantRow) { r.ID = "g-2" },
	} {
		c := row
		change(&c)
		g.setDirect(wRootLab.ID, "boss", c)
		cur, err := cs.grantKey(ctx, wRootLab.ID, "boss", true)
		if err != nil || seen[cur] {
			t.Errorf("%s: the key did not change (%v)", name, err)
		}
		seen[cur] = true
	}
	// A session with direct grants only passes as a grant holder.
	g.setDirect(wAlicePi1.ID, "agent", row)
	if pr, _, key, err := cs.authenticate(ctx, wAlicePi1.ID, "", "agent", ops.OpSendText); err != nil || key == "" || pr.Scope != nil {
		t.Fatalf("direct only: key %q scope %v (%v)", key, pr.Scope, err)
	}
}
