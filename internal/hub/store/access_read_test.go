package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The snapshot holds every table the access model reads, and it is taken
// while another connection holds the write lock: it is a read transaction.
func TestAccessSnapshotReadsEveryTableWithoutTheWriteLock(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	g := f.put(t, f.ownerGrant("s"))
	if err := f.s.PutMasterSession(ctx, MasterRow{WorkerID: f.a.ID, SessionName: "boss", CreatedBy: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutAccept(ctx, AcceptRow{TargetWorkerID: f.b.ID, SourceWorkerID: f.a.ID, Sessions: []string{"x"}, CreatedBy: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	all := true
	if err := f.s.SetWorkerControl(ctx, f.c.ID, nil, &all); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SaveWorkerShare(ctx, f.b.ID, ops.ShareDecl{Rule: ops.ShareRule{Mode: ops.ShareModeSelect, Names: []string{"x", "y"}}, Revision: 3}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SavePendingShare(ctx, PendingShare{Seq: 7, WorkerID: f.b.ID, Rule: ops.ShareRule{Mode: ops.ShareModeSelect, Names: []string{"x"}}, BaseRevision: 3}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO bound_master_session(worker_id,owner_id,session_name,incarnation_id,worker_pub,generation,active,changed_at)
		VALUES(?,?,?,?,?,1,1,1)`, f.b.ID, "alice", "lead", "inc-1", f.b.Pub)
	f.exec(t, `UPDATE users SET active = 0 WHERE id = 'bob'`)

	var snap AccessSnapshot
	err := f.s.Tx(ctx, func(tx *sql.Tx) error {
		// The write lock is held here; an immediate transaction would wait
		// for the busy timeout and fail.
		if _, err := tx.ExecContext(ctx, `UPDATE workers SET name = 'renamed' WHERE id = ?`, f.a.ID); err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var err error
		snap, err = f.s.AccessSnapshot(rctx)
		return err
	})
	if err != nil {
		t.Fatalf("snapshot under a write transaction: %v", err)
	}
	if len(snap.Workers) != 3 || len(snap.Masters) != 1 || len(snap.Accepts) != 1 || len(snap.Bound) != 1 || len(snap.Pending) != 1 || len(snap.Grants) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	for _, w := range snap.Workers {
		if w.Name == "renamed" || w.Pending {
			t.Errorf("the snapshot saw an uncommitted or pending Worker: %+v", w)
		}
	}
	if u := snap.Users["root"]; !u.Active || !u.Admin || u.Username != "root" {
		t.Errorf("root = %+v", u)
	}
	// alice has no role row in this fixture, bob is deactivated: neither may call.
	if snap.Users["alice"].Active || snap.Users["bob"].Active || snap.Users["bob"].Admin {
		t.Errorf("users = %+v", snap.Users)
	}
	if snap.Grants[0].ID != g.ID || snap.Masters[0].SessionName != "boss" || !snap.Control[f.c.ID].AcceptAll ||
		len(snap.Accepts[0].Sessions) != 1 || !snap.Bound[0].Active || snap.Bound[0].IncarnationID != "inc-1" {
		t.Errorf("rows = %+v", snap)
	}
	if d := snap.Shares[f.b.ID]; d.Revision != 3 || !d.Rule.Includes("y") || snap.Pending[0].Rule.Includes("y") || snap.Pending[0].Seq != 7 {
		t.Errorf("shares = %+v pending = %+v", snap.Shares, snap.Pending)
	}
	// The same rows as the single reads.
	grants, err := f.s.ListSessionGrants(ctx, SessionGrantFilter{})
	if err != nil || len(grants) != 1 || grants[0] != snap.Grants[0] {
		t.Errorf("ListSessionGrants = %+v, %v", grants, err)
	}
}
