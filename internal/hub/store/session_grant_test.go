package store

import (
	"context"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

type grantFixture struct {
	s          *Store
	a, b, c    WorkerRow // a, b: alice; c: bob
	admin, bob string
}

func newGrantFixture(t *testing.T) grantFixture {
	t.Helper()
	s := openTest(t)
	ctx := t.Context()
	for _, u := range []string{"alice", "bob", "root"} {
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO users(id,username,created_at,updated_at) VALUES(?,?,1,1)`, u, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO user_roles(user_id,role,source) VALUES('root','admin','local')`); err != nil {
		t.Fatal(err)
	}
	f := grantFixture{s: s, admin: "root", bob: "bob",
		a: worker(ops.NewUUIDv7(), 1, "alice", "a"), b: worker(ops.NewUUIDv7(), 2, "alice", "b"),
		c: worker(ops.NewUUIDv7(), 3, "bob", "c")}
	for _, w := range []WorkerRow{f.a, f.b, f.c} {
		if err := s.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(t, `UPDATE workers SET approval='approved'`)
	return f
}

func (f grantFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.s.DB().ExecContext(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

// ownerGrant is a -> b (both alice); adminGrant is a -> c (alice -> bob).
func (f grantFixture) ownerGrant(session string) SessionGrantRow {
	return SessionGrantRow{SourceWorkerID: f.a.ID, SourceSession: session, TargetWorkerID: f.b.ID, Level: 1,
		Authority: GrantAuthorityOwner, GrantedBy: "alice", SourceOwnerID: "alice", TargetOwnerID: "alice", UpdatedAt: 100}
}

func (f grantFixture) adminGrant(session string) SessionGrantRow {
	return SessionGrantRow{SourceWorkerID: f.a.ID, SourceSession: session, TargetWorkerID: f.c.ID, TargetSession: "t", Level: 3,
		Authority: GrantAuthorityAdmin, GrantedBy: f.admin, SourceOwnerID: "alice", TargetOwnerID: "bob", UpdatedAt: 100}
}

func (f grantFixture) put(t *testing.T, g SessionGrantRow) SessionGrantRow {
	t.Helper()
	out, err := f.s.PutSessionGrant(context.Background(), g, 0, GrantWrite{ActorID: g.GrantedBy})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSessionGrantCRUDAndRevision(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	g := f.put(t, f.ownerGrant("boss"))
	if g.ID == "" || g.Revision != 1 || g.Level != 1 || g.ExpiresAt != nil || g.SuspendedAt != nil || g.CreatedAt != 100 {
		t.Fatalf("created = %+v", g)
	}
	exp := int64(5000)
	upd := SessionGrantRow{ID: g.ID, Level: 2, ExpiresAt: &exp, Note: "n", UpdatedAt: 200}
	if _, err := f.s.PutSessionGrant(ctx, upd, 7, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantRevision) {
		t.Fatalf("stale revision: %v", err)
	}
	if _, err := f.s.PutSessionGrant(ctx, upd, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantRevision) {
		t.Fatalf("create over an existing id: %v", err)
	}
	g2, err := f.s.PutSessionGrant(ctx, upd, 1, GrantWrite{ActorID: "alice"})
	if err != nil || g2.Revision != 2 || g2.Level != 2 || g2.ExpiresAt == nil || *g2.ExpiresAt != 5000 || g2.Note != "n" ||
		g2.UpdatedAt != 200 || g2.SourceSession != "boss" || g2.GrantedBy != "alice" {
		t.Fatalf("updated = %+v, %v", g2, err)
	}
	if _, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: ops.NewUUIDv7(), Level: 1}, 1, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing grant: %v", err)
	}
	f.put(t, f.adminGrant("boss"))
	if got, err := f.s.SessionGrant(ctx, g.ID); err != nil || got.Revision != 2 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	for _, c := range []struct {
		f    SessionGrantFilter
		want int
	}{{SessionGrantFilter{}, 2}, {SessionGrantFilter{WorkerID: f.c.ID}, 1}, {SessionGrantFilter{OwnerID: "bob"}, 1},
		{SessionGrantFilter{OwnerID: "alice"}, 2}, {SessionGrantFilter{WorkerID: f.b.ID, OwnerID: "bob"}, 0}} {
		if rows, err := f.s.ListSessionGrants(ctx, c.f); err != nil || len(rows) != c.want {
			t.Errorf("list %+v = %d rows, %v; want %d", c.f, len(rows), err, c.want)
		}
	}
	if rows, err := f.s.SessionGrantsFor(ctx, f.a.ID, "boss"); err != nil || len(rows) != 2 {
		t.Fatalf("grants for boss = %+v, %v", rows, err)
	}
	if rows, _ := f.s.SessionGrantsFor(ctx, f.a.ID, "other"); len(rows) != 0 {
		t.Fatalf("grants for another session = %+v", rows)
	}
	if ok, err := f.s.DeleteSessionGrant(ctx, g.ID); err != nil || !ok {
		t.Fatalf("delete: %t, %v", ok, err)
	}
	if ok, _ := f.s.DeleteSessionGrant(ctx, g.ID); ok {
		t.Fatal("second delete reported a row")
	}
	if _, err := f.s.SessionGrant(ctx, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestSessionGrantRefusals(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	f.put(t, f.ownerGrant("boss"))
	if _, err := f.s.PutSessionGrant(ctx, f.ownerGrant("boss"), 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantExists) {
		t.Fatalf("second grant for the same pair: %v", err)
	}
	same := f.ownerGrant("x")
	same.TargetWorkerID = f.a.ID
	if _, err := f.s.PutSessionGrant(ctx, same, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("same Worker: %v", err)
	}
	// The table refuses it too, whatever the store checks.
	if _, err := f.s.DB().ExecContext(ctx, `INSERT INTO session_grant(id,source_worker_id,source_session,target_worker_id,level,
		authority,granted_by,source_owner_id,target_owner_id,created_at,updated_at) VALUES('x',?,'s',?,1,'owner','alice','alice','alice',1,1)`,
		f.a.ID, f.a.ID); err == nil {
		t.Fatal("the table stored a grant inside one Worker")
	}
	if _, err := f.s.DB().ExecContext(ctx, `INSERT INTO session_grant(id,source_worker_id,source_session,target_worker_id,level,
		authority,granted_by,source_owner_id,target_owner_id,created_at,updated_at) VALUES('y',?,'s',?,1,'owner','alice','alice','bob',1,1)`,
		f.a.ID, f.c.ID); err == nil {
		t.Fatal("the table stored an owner grant across two owners")
	}
	cross := f.adminGrant("x")
	cross.Authority = GrantAuthorityOwner
	if _, err := f.s.PutSessionGrant(ctx, cross, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantOwners) {
		t.Fatalf("owner grant across two owners: %v", err)
	}
	stale := f.adminGrant("x")
	stale.TargetOwnerID = "alice"
	if _, err := f.s.PutSessionGrant(ctx, stale, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantOwners) {
		t.Fatalf("owner not as decided: %v", err)
	}
	bad := f.ownerGrant("lvl")
	bad.Level = 4
	if _, err := f.s.PutSessionGrant(ctx, bad, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("level 4: %v", err)
	}
	f.exec(t, `UPDATE workers SET approval='pending' WHERE id=?`, f.c.ID)
	if _, err := f.s.PutSessionGrant(ctx, f.adminGrant("x"), 0, GrantWrite{ActorID: f.admin}); !errors.Is(err, ErrGrantOwners) {
		t.Fatalf("unapproved Worker: %v", err)
	}
}

func TestSessionGrantFollowsItsWorkers(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	own := f.put(t, f.ownerGrant("boss"))
	adm := f.put(t, f.adminGrant("boss"))
	f.exec(t, `DELETE FROM workers WHERE id=?`, f.c.ID)
	if _, err := f.s.SessionGrant(ctx, adm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant survived its Worker: %v", err)
	}
	if _, err := f.s.SessionGrant(ctx, own.ID); err != nil {
		t.Fatalf("unrelated grant: %v", err)
	}
	f.exec(t, `UPDATE workers SET owner_id='bob' WHERE id=?`, f.b.ID)
	if _, err := f.s.SessionGrant(ctx, own.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant survived an owner change: %v", err)
	}
}

func TestSessionGrantSuspension(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	own := f.put(t, f.ownerGrant("boss"))
	adm := f.put(t, f.adminGrant("boss"))
	// A second admin row keeps the authority.
	f.exec(t, `INSERT INTO user_roles(user_id,role,source) VALUES('root','admin','idp')`)
	f.exec(t, `DELETE FROM user_roles WHERE user_id='root' AND source='idp'`)
	if g, _ := f.s.SessionGrant(ctx, adm.ID); g.SuspendedAt != nil {
		t.Fatalf("suspended while still an administrator: %+v", g)
	}
	f.exec(t, `UPDATE user_roles SET role='user' WHERE user_id='root'`)
	g, err := f.s.SessionGrant(ctx, adm.ID)
	if err != nil || g.SuspendedAt == nil || *g.SuspendedAt <= 0 || g.SuspendCause != "grantor_not_admin" || g.Revision != 2 {
		t.Fatalf("after demotion = %+v, %v", g, err)
	}
	f.exec(t, `UPDATE user_roles SET role='admin' WHERE user_id='root'`)
	if g, _ = f.s.SessionGrant(ctx, adm.ID); g.SuspendedAt == nil {
		t.Fatal("the returning role lifted the suspension")
	}
	if o, _ := f.s.SessionGrant(ctx, own.ID); o.SuspendedAt != nil {
		t.Fatalf("owner grant suspended: %+v", o)
	}
	// Only an active administrator approves; the store reads the role itself.
	if _, err := f.s.ApproveSessionGrant(ctx, adm.ID, 2, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("approve by a plain user: %v", err)
	}
	f.exec(t, `INSERT INTO user_roles(user_id,role,source) VALUES('alice','admin','local')`)
	if _, err := f.s.ApproveSessionGrant(ctx, adm.ID, 1, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantRevision) {
		t.Fatalf("approve with a stale revision: %v", err)
	}
	g, err = f.s.ApproveSessionGrant(ctx, adm.ID, 2, GrantWrite{ActorID: "alice"})
	if err != nil || g.SuspendedAt != nil || g.SuspendCause != "" || g.GrantedBy != "alice" || g.Revision != 3 {
		t.Fatalf("approved = %+v, %v", g, err)
	}
	if _, err := f.s.ApproveSessionGrant(ctx, "missing", 1, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve a missing grant: %v", err)
	}

	// Deleting the last admin row suspends too; deactivation has its own cause.
	del := f.put(t, func() SessionGrantRow { r := f.adminGrant("del"); return r }())
	f.exec(t, `DELETE FROM user_roles WHERE user_id='root' AND role='admin'`)
	if g, _ = f.s.SessionGrant(ctx, del.ID); g.SuspendCause != "grantor_not_admin" {
		t.Fatalf("after role deletion = %+v", g)
	}
	off := f.adminGrant("off")
	off.GrantedBy = "bob"
	f.exec(t, `INSERT INTO user_roles(user_id,role,source) VALUES('bob','admin','local')`)
	offRow := f.put(t, off)
	f.exec(t, `UPDATE users SET active=0 WHERE id='bob'`)
	g, _ = f.s.SessionGrant(ctx, offRow.ID)
	if g.SuspendedAt == nil || g.SuspendCause != "grantor_inactive" {
		t.Fatalf("after deactivation = %+v", g)
	}
	f.exec(t, `UPDATE users SET active=1 WHERE id='bob'`)
	if g, _ = f.s.SessionGrant(ctx, offRow.ID); g.SuspendedAt == nil {
		t.Fatal("reactivation lifted the suspension")
	}
}

func TestSessionGrantPurgeAndExpiry(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	src := f.put(t, f.ownerGrant("gone"))    // source session gone on a
	whole := f.put(t, f.ownerGrant("stays")) // whole-Worker target, live source
	late := f.ownerGrant("late")             // saved after the evidence
	late.UpdatedAt = 500
	lateRow := f.put(t, late)
	tgt := f.adminGrant("stays") // target session "t" on c
	tgtRow := f.put(t, tgt)
	otherSide := SessionGrantRow{SourceWorkerID: f.b.ID, SourceSession: "s", TargetWorkerID: f.a.ID, TargetSession: "late", Level: 1,
		Authority: GrantAuthorityOwner, GrantedBy: "alice", SourceOwnerID: "alice", TargetOwnerID: "alice", UpdatedAt: 100}
	otherRow := f.put(t, otherSide)

	// "t" is gone on a, not on c: the grant naming c's "t" stays.
	if n, err := f.s.PurgeSessionGrants(ctx, f.a.ID, []string{"t"}, 300); err != nil || n != 0 {
		t.Fatalf("purge of another Worker's name: %d, %v", n, err)
	}
	// The existing session purge takes grants with it, under the same fence.
	if _, err := f.s.PurgeGoneSessions(ctx, f.a.ID, []string{"gone", "late"}, 300); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SessionGrant(ctx, src.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant of a gone source: %v", err)
	}
	if _, err := f.s.SessionGrant(ctx, otherRow.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant on a gone target: %v", err)
	}
	for _, id := range []string{whole.ID, lateRow.ID, tgtRow.ID} {
		if _, err := f.s.SessionGrant(ctx, id); err != nil {
			t.Fatalf("grant %s was purged: %v", id, err)
		}
	}
	if n, err := f.s.PurgeSessionsNotIn(ctx, f.c.ID, []string{"live"}, 300); err != nil || n != 1 {
		t.Fatalf("resync purge: %d, %v", n, err)
	}
	if _, err := f.s.SessionGrant(ctx, tgtRow.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant on a session missing after resync: %v", err)
	}

	exp := int64(1000)
	if _, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: whole.ID, Level: 1, ExpiresAt: &exp}, 1, GrantWrite{ActorID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.DeleteExpiredSessionGrants(ctx, 1000); err != nil || n != 0 {
		t.Fatalf("sweep at the expiry time: %d, %v", n, err)
	}
	if n, err := f.s.DeleteExpiredSessionGrants(ctx, 1001); err != nil || n != 1 {
		t.Fatalf("sweep: %d, %v", n, err)
	}
	if rows, _ := f.s.ListSessionGrants(ctx, SessionGrantFilter{}); len(rows) != 1 || rows[0].ID != lateRow.ID {
		t.Fatalf("left = %+v", rows)
	}
}

// The acting user's authority is read in the writing transaction: a demoted
// or deactivated administrator, a plain user between two owners and a user
// who does not own both Workers of an owner grant commit nothing.
func TestSessionGrantWriteNeedsAuthorityInTheTransaction(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	adm := f.put(t, f.adminGrant("boss"))
	own := f.put(t, f.ownerGrant("boss"))
	for _, actor := range []string{"", "nobody", "alice", "bob"} {
		if _, err := f.s.PutSessionGrant(ctx, f.adminGrant("x"), 0, GrantWrite{ActorID: actor}); !errors.Is(err, ErrGrantAuthority) {
			t.Fatalf("create of an admin grant by %q: %v", actor, err)
		}
	}
	if _, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: own.ID, Level: 3}, 1, GrantWrite{ActorID: "bob"}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("update of an owner grant by another user: %v", err)
	}
	// An administrator creates an owner grant for the owner; it names the
	// administrator as granted_by whatever the row says.
	forAlice := f.ownerGrant("for-alice")
	g, err := f.s.PutSessionGrant(ctx, forAlice, 0, GrantWrite{ActorID: f.admin})
	if err != nil || g.Authority != GrantAuthorityOwner || g.GrantedBy != f.admin {
		t.Fatalf("owner grant by an administrator = %+v, %v", g, err)
	}
	// The demotion commits before the write: nothing changes.
	f.exec(t, `DELETE FROM user_roles WHERE user_id='root' AND role='admin'`)
	cur, _ := f.s.SessionGrant(ctx, adm.ID)
	if _, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: adm.ID, Level: 1}, cur.Revision, GrantWrite{ActorID: f.admin}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("update by a demoted administrator: %v", err)
	}
	if _, err := f.s.ApproveSessionGrant(ctx, adm.ID, cur.Revision, GrantWrite{ActorID: f.admin}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("approve by a demoted administrator: %v", err)
	}
	if _, err := f.s.PutSessionGrant(ctx, f.adminGrant("y"), 0, GrantWrite{ActorID: f.admin}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("create by a demoted administrator: %v", err)
	}
	if after, _ := f.s.SessionGrant(ctx, adm.ID); after.Revision != cur.Revision || after.Level != 3 || after.SuspendedAt == nil {
		t.Fatalf("after the refused writes = %+v", after)
	}
	// An inactive owner cannot edit an own grant.
	f.exec(t, `UPDATE users SET active=0 WHERE id='alice'`)
	if _, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: own.ID, Level: 3}, 1, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("update by an inactive owner: %v", err)
	}
}

// A revoke with a stale revision leaves the grant; the audit row commits with
// the change and a failing audit row takes the change back.
func TestSessionGrantRevokeRevisionAndAudit(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	g := f.put(t, f.ownerGrant("boss"))
	g2, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: g.ID, Level: 2}, 1, GrantWrite{ActorID: "alice"})
	if err != nil || g2.Revision != 2 {
		t.Fatalf("update = %+v, %v", g2, err)
	}
	if _, err := f.s.RevokeSessionGrant(ctx, g.ID, 1, GrantWrite{}); !errors.Is(err, ErrGrantRevision) {
		t.Fatalf("revoke at an old revision: %v", err)
	}
	// A revoke without a revision is refused too: it would delete a grant
	// changed after the caller looked at it.
	if _, err := f.s.RevokeSessionGrant(ctx, g.ID, 0, GrantWrite{}); !errors.Is(err, ErrGrantRevision) {
		t.Fatalf("revoke without a revision: %v", err)
	}
	if _, err := f.s.SessionGrant(ctx, g.ID); err != nil {
		t.Fatalf("the grant is gone after a stale revoke: %v", err)
	}
	boom := errors.New("no audit")
	fail := GrantWrite{ActorID: "alice", Audit: func(*SessionGrantRow, SessionGrantRow) (AuditRow, error) { return AuditRow{}, boom }}
	if _, err := f.s.RevokeSessionGrant(ctx, g.ID, 2, fail); !errors.Is(err, boom) {
		t.Fatalf("revoke with a failing audit: %v", err)
	}
	if _, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: g.ID, Level: 3}, 2, fail); !errors.Is(err, boom) {
		t.Fatalf("update with a failing audit: %v", err)
	}
	if _, err := f.s.PutSessionGrant(ctx, f.ownerGrant("other"), 0, fail); !errors.Is(err, boom) {
		t.Fatalf("create with a failing audit: %v", err)
	}
	if rows, _ := f.s.ListSessionGrants(ctx, SessionGrantFilter{}); len(rows) != 1 || rows[0].Revision != 2 || rows[0].Level != 2 {
		t.Fatalf("after the failed audits = %+v", rows)
	}
	var seenOld *SessionGrantRow
	ok := GrantWrite{ActorID: "alice", Audit: func(old *SessionGrantRow, now SessionGrantRow) (AuditRow, error) {
		seenOld = old
		return AuditRow{Actor: "alice", Action: "grant.revoke", Outcome: "success", GrantID: now.ID}, nil
	}}
	gone, err := f.s.RevokeSessionGrant(ctx, g.ID, 2, ok)
	if err != nil || gone.ID != g.ID || seenOld == nil || seenOld.Revision != 2 {
		t.Fatalf("revoke = %+v, %v (old %+v)", gone, err, seenOld)
	}
	limit := 10
	rows, err := f.s.ListAudit(ctx, AuditQuery{Action: "grant.revoke", Limit: limit})
	if err != nil || len(rows) != 1 || rows[0].GrantID != g.ID || rows[0].ID == "" || rows[0].TS == 0 {
		t.Fatalf("audit rows = %+v, %v", rows, err)
	}
	if _, err := f.s.RevokeSessionGrant(ctx, g.ID, 2, GrantWrite{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
}

// An edit or an approval does not move the purge fence: a grant saved again
// after the Hub saw its session end is purged all the same.
func TestSessionGrantEditDoesNotMoveThePurgeFence(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	g := f.put(t, f.ownerGrant("gone")) // created at 100
	if _, err := f.s.PutSessionGrant(ctx, SessionGrantRow{ID: g.ID, Level: 1, Note: "kept?", UpdatedAt: 900}, 1, GrantWrite{ActorID: "alice"}); err != nil {
		t.Fatal(err)
	}
	adm := f.put(t, f.adminGrant("gone"))
	// Only a suspended grant can be approved: the grantor loses the role and
	// gets it back, which leaves the grant suspended at revision 2.
	if _, err := f.s.ApproveSessionGrant(ctx, adm.ID, 1, GrantWrite{ActorID: f.admin}); !errors.Is(err, ErrGrantNotSuspended) {
		t.Fatalf("approve of a grant that is not suspended: %v", err)
	}
	if cur, _ := f.s.SessionGrant(ctx, adm.ID); cur.Revision != 1 || cur.GrantedBy != f.admin || len(grantAudit(t, f.s, adm.ID)) != 0 {
		t.Fatalf("the refused approval changed the grant: %+v", cur)
	}
	f.exec(t, `UPDATE user_roles SET role='user' WHERE user_id='root'`)
	f.exec(t, `UPDATE user_roles SET role='admin' WHERE user_id='root'`)
	if _, err := f.s.ApproveSessionGrant(ctx, adm.ID, 2, GrantWrite{ActorID: f.admin}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeSessionGrants(ctx, f.a.ID, []string{"gone"}, 300); err != nil || n != 2 {
		t.Fatalf("purge after an edit and an approval: %d, %v", n, err)
	}
	// A grant created after the evidence stays.
	late := f.ownerGrant("gone")
	late.UpdatedAt = 500
	lateRow := f.put(t, late)
	if n, err := f.s.PurgeSessionGrants(ctx, f.a.ID, []string{"gone"}, 300); err != nil || n != 0 {
		t.Fatalf("purge of a later grant: %d, %v", n, err)
	}
	if _, err := f.s.SessionGrant(ctx, lateRow.ID); err != nil {
		t.Fatal(err)
	}
}
