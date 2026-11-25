package store

import (
	"errors"
	"sort"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// grantAudit returns the audit rows of one grant, newest first.
func grantAudit(t *testing.T, s *Store, id string) []AuditRow {
	t.Helper()
	rows, err := s.ListAudit(t.Context(), AuditQuery{GrantID: id, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// wantSystemRow checks the one system row of a grant.
func wantSystemRow(t *testing.T, s *Store, g SessionGrantRow, action, detail string) {
	t.Helper()
	rows := grantAudit(t, s, g.ID)
	if len(rows) != 1 {
		t.Fatalf("%s: %d audit rows of grant %s, want 1: %+v", detail, len(rows), g.ID, rows)
	}
	r := rows[0]
	target := g.TargetWorkerID + ":*"
	if g.TargetSession != "" {
		target = g.TargetWorkerID + ":" + g.TargetSession
	}
	if r.Actor != AuditActorSystem || r.Action != action || r.Detail != detail || r.Outcome != "success" ||
		r.Source != g.SourceWorkerID+":"+g.SourceSession || r.Target != target || r.GrantID != g.ID || r.ActingAs != "" {
		t.Fatalf("%s: row = %+v", detail, r)
	}
	if !ops.ValidUUIDv7(r.ID) {
		t.Fatalf("%s: audit id %q is not a UUIDv7", detail, r.ID)
	}
	if r.TS < 1_600_000_000_000 {
		t.Fatalf("%s: ts %d is not unix milliseconds of now", detail, r.TS)
	}
}

func TestGrantEndedByTriggersIsAudited(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	own := f.put(t, f.ownerGrant("boss"))   // a -> b
	own2 := f.put(t, f.ownerGrant("other")) // a -> b
	adm := f.put(t, f.adminGrant("boss"))   // a -> c:t

	// Worker c is removed: its grant goes by foreign key and is recorded.
	f.exec(t, `DELETE FROM workers WHERE id=?`, f.c.ID)
	if _, err := f.s.SessionGrant(ctx, adm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant survived its Worker: %v", err)
	}
	wantSystemRow(t, f.s, adm, AuditActionGrantEnd, "cause=worker_removed")
	if n := len(grantAudit(t, f.s, own.ID)); n != 0 {
		t.Fatalf("unrelated grant has %d audit rows", n)
	}

	// An update that keeps the owner records nothing.
	f.exec(t, `UPDATE workers SET owner_id='alice' WHERE id=?`, f.b.ID)
	if n := len(grantAudit(t, f.s, own.ID)); n != 0 {
		t.Fatalf("same owner: %d audit rows", n)
	}
	// Worker b changes owner: both grants end, one row each, distinct ids.
	f.exec(t, `UPDATE workers SET owner_id='bob' WHERE id=?`, f.b.ID)
	if _, err := f.s.SessionGrant(ctx, own.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant survived an owner change: %v", err)
	}
	wantSystemRow(t, f.s, own, AuditActionGrantEnd, "cause=owner_changed")
	wantSystemRow(t, f.s, own2, AuditActionGrantEnd, "cause=owner_changed")
}

func TestGrantSuspendedByTriggersIsAudited(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	own := f.put(t, f.ownerGrant("boss"))
	adm := f.put(t, f.adminGrant("boss"))
	f.exec(t, `UPDATE user_roles SET role='user' WHERE user_id='root'`)
	wantSystemRow(t, f.s, adm, AuditActionGrantSuspend, "cause=grantor_not_admin")
	if n := len(grantAudit(t, f.s, own.ID)); n != 0 {
		t.Fatalf("owner grant has %d audit rows", n)
	}
	// Already suspended: a second cause changes nothing and records nothing.
	f.exec(t, `UPDATE users SET active=0 WHERE id='root'`)
	wantSystemRow(t, f.s, adm, AuditActionGrantSuspend, "cause=grantor_not_admin")

	// Approved again, then the grantor is deactivated.
	g, err := f.s.SessionGrant(ctx, adm.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Only an active administrator may approve (read in the approving
	// transaction): alice gets the role first.
	f.exec(t, `INSERT INTO user_roles(user_id,role,source) VALUES('alice','admin','local')`)
	if _, err := f.s.ApproveSessionGrant(ctx, adm.ID, g.Revision, GrantWrite{ActorID: "alice"}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE users SET active=0 WHERE id='alice'`)
	rows := grantAudit(t, f.s, adm.ID)
	if len(rows) != 2 {
		t.Fatalf("after the second suspension: %+v", rows)
	}
	var causes []string
	for _, r := range rows {
		if r.Action != AuditActionGrantSuspend || r.Actor != AuditActorSystem {
			t.Fatalf("row = %+v", r)
		}
		causes = append(causes, r.Detail)
	}
	sort.Strings(causes)
	if causes[0] != "cause=grantor_inactive" || causes[1] != "cause=grantor_not_admin" {
		t.Fatalf("causes = %v", causes)
	}
}

func TestGrantEndedByPurgeAndExpiryIsAudited(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	src := f.put(t, f.ownerGrant("gone"))    // source session gone on a
	whole := f.put(t, f.ownerGrant("stays")) // whole-Worker target, live source
	late := f.ownerGrant("late")             // saved after the evidence
	late.UpdatedAt = 500
	lateRow := f.put(t, late)
	tgtRow := f.put(t, f.adminGrant("stays")) // target session "t" on c

	if n, err := f.s.EndGoneSessionGrants(ctx, f.a.ID, []string{"gone", "late"}, 300); err != nil || n != 1 {
		t.Fatalf("end gone: %d, %v", n, err)
	}
	if _, err := f.s.SessionGrant(ctx, src.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant of a gone source: %v", err)
	}
	wantSystemRow(t, f.s, src, AuditActionGrantEnd, "cause=session_gone")
	for _, g := range []SessionGrantRow{whole, lateRow, tgtRow} {
		if _, err := f.s.SessionGrant(ctx, g.ID); err != nil {
			t.Fatalf("grant %s was ended: %v", g.ID, err)
		}
		if n := len(grantAudit(t, f.s, g.ID)); n != 0 {
			t.Fatalf("kept grant has %d audit rows", n)
		}
	}
	// A second run finds nothing and writes nothing.
	if n, err := f.s.EndGoneSessionGrants(ctx, f.a.ID, []string{"gone"}, 300); err != nil || n != 0 {
		t.Fatalf("second run: %d, %v", n, err)
	}
	wantSystemRow(t, f.s, src, AuditActionGrantEnd, "cause=session_gone")

	if n, err := f.s.EndSessionGrantsNotIn(ctx, f.c.ID, []string{"live"}, 300); err != nil || n != 1 {
		t.Fatalf("resync: %d, %v", n, err)
	}
	wantSystemRow(t, f.s, tgtRow, AuditActionGrantEnd, "cause=session_gone")

	// Expiry: only the grant that expired before the cut.
	old, fresh := int64(1000), int64(9000)
	e1 := f.ownerGrant("old")
	e1.ExpiresAt = &old
	e1Row := f.put(t, e1)
	e2 := f.ownerGrant("fresh")
	e2.ExpiresAt = &fresh
	e2Row := f.put(t, e2)
	if n, err := f.s.EndExpiredSessionGrants(ctx, 5000); err != nil || n != 1 {
		t.Fatalf("expired: %d, %v", n, err)
	}
	wantSystemRow(t, f.s, e1Row, AuditActionGrantEnd, "cause=expired")
	if _, err := f.s.SessionGrant(ctx, e2Row.ID); err != nil {
		t.Fatalf("unexpired grant: %v", err)
	}
	if _, err := f.s.SessionGrant(ctx, whole.ID); err != nil {
		t.Fatalf("grant without expiry: %v", err)
	}
}

func TestTouchSessionGrants(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	a := f.put(t, f.ownerGrant("one"))
	b := f.put(t, f.ownerGrant("two"))
	c := f.put(t, f.ownerGrant("three"))
	if err := f.s.TouchSessionGrants(ctx, []string{a.ID, b.ID, a.ID, "missing", ""}, 2000, map[string]int64{a.ID: 5}); err != nil {
		t.Fatal(err)
	}
	// An older time adds to the count and keeps the later time.
	if err := f.s.TouchSessionGrants(ctx, []string{a.ID}, 1500, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.TouchSessionGrants(ctx, nil, 1, nil); err != nil {
		t.Fatal(err)
	}
	use, err := f.s.SessionGrantUsage(ctx, a.ID, b.ID, c.ID, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if u := use[a.ID]; u.UseCount != 6 || u.LastUsedAt == nil || *u.LastUsedAt != 2000 {
		t.Fatalf("a = %+v", u)
	}
	if u := use[b.ID]; u.UseCount != 1 || u.LastUsedAt == nil || *u.LastUsedAt != 2000 {
		t.Fatalf("b = %+v", u)
	}
	if u, ok := use[c.ID]; !ok || u.UseCount != 0 || u.LastUsedAt != nil {
		t.Fatalf("c = %+v, %v", u, ok)
	}
	if _, ok := use["missing"]; ok {
		t.Fatal("usage of a missing grant")
	}
	// Usage is not a change of the grant: revision and the purge fence stay.
	g, err := f.s.SessionGrant(ctx, a.ID)
	if err != nil || g.Revision != a.Revision || g.UpdatedAt != a.UpdatedAt {
		t.Fatalf("grant after use = %+v, %v", g, err)
	}
}

func TestListAuditByGrant(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for i, g := range []string{"g1", "", "g1", "g2"} {
		if err := s.InsertAudit(ctx, nil, AuditRow{ID: NewID(), TS: int64(100 + i), Actor: "u", Action: "x", Outcome: "success", GrantID: g}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListAudit(ctx, AuditQuery{GrantID: "g1", Limit: 10})
	if err != nil || len(rows) != 2 || rows[0].TS != 102 || rows[1].TS != 100 {
		t.Fatalf("g1 = %+v, %v", rows, err)
	}
	if rows, err = s.ListAudit(ctx, AuditQuery{Limit: 10}); err != nil || len(rows) != 4 {
		t.Fatalf("no filter = %d, %v", len(rows), err)
	}
	if rows, err = s.ListAudit(ctx, AuditQuery{GrantID: "none", Limit: 10}); err != nil || len(rows) != 0 {
		t.Fatalf("unknown grant = %d, %v", len(rows), err)
	}
}
