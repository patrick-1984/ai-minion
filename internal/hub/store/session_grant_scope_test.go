package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func grantIDsOf(rows []SessionGrantRow) []string {
	out := make([]string, 0, len(rows))
	for _, g := range rows {
		out = append(out, g.ID)
	}
	return out
}

// The three source scopes: shape, authority, uniqueness and the lookup of a
// calling session.
func TestSessionGrantSourceScopes(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()

	// Every shared session of Worker a reaches minion-5 on b, with the flags
	// saved at create.
	wide := f.ownerGrant("")
	wide.SourceScope, wide.TargetSession, wide.Level = GrantSourceWorker, "minion-5", 3
	wide.FilesPull, wide.FilesPush = true, true
	wideRow := f.put(t, wide)
	if wideRow.SourceScope != GrantSourceWorker || wideRow.SourceSession != "" || !wideRow.FilesPull || !wideRow.FilesPush {
		t.Fatalf("worker-wide grant = %+v", wideRow)
	}
	if _, err := f.s.PutSessionGrant(ctx, wide, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantExists) {
		t.Fatalf("second worker-wide grant: %v", err)
	}

	// Everyone needs admin authority and an administrator.
	every := SessionGrantRow{SourceScope: GrantSourceEveryone, TargetWorkerID: f.c.ID, TargetSession: "shared", Level: 3,
		Authority: GrantAuthorityAdmin, TargetOwnerID: "bob", UpdatedAt: 100}
	if _, err := f.s.PutSessionGrant(ctx, every, 0, GrantWrite{ActorID: "bob"}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("everyone by an owner: %v", err)
	}
	ownerEvery := every
	ownerEvery.Authority = GrantAuthorityOwner
	if _, err := f.s.PutSessionGrant(ctx, ownerEvery, 0, GrantWrite{ActorID: f.admin}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("everyone with owner authority: %v", err)
	}
	everyRow, err := f.s.PutSessionGrant(ctx, every, 0, GrantWrite{ActorID: f.admin})
	if err != nil || everyRow.SourceWorkerID != "" || everyRow.SourceOwnerID != "" || everyRow.Scope() != GrantSourceEveryone {
		t.Fatalf("everyone grant = %+v, %v", everyRow, err)
	}
	if _, err := f.s.PutSessionGrant(ctx, every, 0, GrantWrite{ActorID: f.admin}); !errors.Is(err, ErrGrantExists) {
		t.Fatalf("second everyone grant: %v", err)
	}
	if GrantSourceAudit(everyRow) != "*" || GrantSourceAudit(wideRow) != f.a.ID+":*" {
		t.Fatalf("audit sources = %q, %q", GrantSourceAudit(everyRow), GrantSourceAudit(wideRow))
	}

	named := f.put(t, f.ownerGrant("wb-2"))

	// A session of a matches its named grant, the Worker-wide one and
	// everyone; another session of a only the last two; a session of c (the
	// everyone target's own Worker) none of them.
	got, err := f.s.SessionGrantsFor(ctx, f.a.ID, "wb-2")
	if err != nil {
		t.Fatal(err)
	}
	if ids := grantIDsOf(got); !slices.Contains(ids, named.ID) || !slices.Contains(ids, wideRow.ID) || !slices.Contains(ids, everyRow.ID) || len(ids) != 3 {
		t.Fatalf("grants of a:wb-2 = %v", ids)
	}
	got, _ = f.s.SessionGrantsFor(ctx, f.a.ID, "new-session")
	if ids := grantIDsOf(got); len(ids) != 2 || slices.Contains(ids, named.ID) {
		t.Fatalf("grants of a:new-session = %v", ids)
	}
	got, _ = f.s.SessionGrantsFor(ctx, f.c.ID, "any")
	if len(got) != 0 {
		t.Fatalf("grants of c:any = %v", grantIDsOf(got))
	}
	got, _ = f.s.SessionGrantsFor(ctx, f.b.ID, "any")
	if ids := grantIDsOf(got); len(ids) != 1 || ids[0] != everyRow.ID {
		t.Fatalf("grants of b:any = %v", ids)
	}

	// A gone session ends only the grant that names it.
	if n, err := f.s.EndGoneSessionGrants(ctx, f.a.ID, []string{"wb-2"}, 1<<62); err != nil || n != 1 {
		t.Fatalf("ended = %d, %v", n, err)
	}
	if _, err := f.s.SessionGrant(ctx, wideRow.ID); err != nil {
		t.Fatalf("worker-wide grant after a lost session: %v", err)
	}
	if _, err := f.s.SessionGrant(ctx, everyRow.ID); err != nil {
		t.Fatalf("everyone grant after a lost session: %v", err)
	}
	// The deprecated bind refuses a grant with no named source.
	if _, err := f.s.BindSessionGrant(ctx, wideRow.ID, "inc", wideRow.Revision, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("bind a worker-wide grant: %v", err)
	}
	// A removed target Worker ends the everyone grant.
	f.exec(t, `DELETE FROM workers WHERE id = ?`, f.c.ID)
	if _, err := f.s.SessionGrant(ctx, everyRow.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("everyone grant after its target was removed: %v", err)
	}
}

// Several targets are created in one transaction: all or nothing, one
// receipt.
func TestPutSessionGrantsAllOrNothing(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	existing := f.ownerGrant("wb-2")
	existing.TargetSession = "taken"
	f.put(t, existing)

	rows := func(names ...string) []SessionGrantRow {
		var out []SessionGrantRow
		for _, n := range names {
			r := f.ownerGrant("wb-2")
			r.TargetSession, r.Level, r.FilesPull = n, 3, true
			out = append(out, r)
		}
		return out
	}
	receipts := 0
	w := GrantWrite{ActorID: "alice", ReceiptMany: func(now []SessionGrantRow) (*OpRequestRow, error) {
		receipts++
		return nil, nil
	}}
	if _, err := f.s.PutSessionGrants(ctx, rows("one", "taken"), w); !errors.Is(err, ErrGrantExists) {
		t.Fatalf("a set with a taken pair: %v", err)
	}
	all, _ := f.s.ListSessionGrants(ctx, SessionGrantFilter{})
	if len(all) != 1 || receipts != 0 {
		t.Fatalf("after a refused set: %d grants, %d receipts", len(all), receipts)
	}
	got, err := f.s.PutSessionGrants(ctx, rows("one", "two"), w)
	if err != nil || len(got) != 2 || got[0].TargetSession != "one" || got[1].TargetSession != "two" || !got[1].FilesPull || receipts != 1 {
		t.Fatalf("created = %+v, %v, receipts %d", got, err, receipts)
	}
}

// Migration 0045 keeps every grant with its id, flags and revision as a
// one-session grant, a bound grant's incarnation as its restriction, and the
// acceptance flags that were on as bound-only.
func TestGrantSourceScopeMigration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	old, err := Open(dir, WithMigrations(subsetMigrations(t, "0045"), migrationsDir))
	if err != nil {
		t.Fatal(err)
	}
	a, b := ops.NewUUIDv7(), ops.NewUUIDv7()
	for _, q := range []string{
		`INSERT INTO users(id,username,created_at,updated_at) VALUES('alice','alice',1,1)`,
		`INSERT INTO user_roles(user_id,role,source) VALUES('alice','user','local')`,
	} {
		if _, err := old.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{a, b} {
		if err := old.InsertWorker(ctx, nil, worker(id, byte(i+1), "alice", []string{"wa", "wb"}[i])); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`UPDATE workers SET approval='approved'`,
		`INSERT INTO session_grant (id, source_worker_id, source_session, source_incarnation, target_worker_id, target_session,
			level, authority, granted_by, source_owner_id, target_owner_id, revision, created_at, updated_at, files_pull, files_push)
			VALUES ('g-bound', '` + a + `', 'boss', 'inc-1', '` + b + `', 't', 3, 'owner', 'alice', 'alice', 'alice', 4, 1, 1, 1, 1),
			       ('g-bound2', '` + a + `', 'boss', 'inc-2', '` + b + `', 't', 1, 'owner', 'alice', 'alice', 'alice', 1, 1, 1, 0, 0),
			       ('g-named', '` + a + `', 'boss', '', '` + b + `', '', 2, 'owner', 'alice', 'alice', 'alice', 2, 1, 1, 0, 0)`,
		`INSERT INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at, files_pull)
			VALUES ('` + b + `', '` + a + `', '["t"]', 'alice', 1, 1)`,
	} {
		if _, err := old.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = old.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	g, err := s.SessionGrant(ctx, "g-bound")
	if err != nil || g.Scope() != GrantSourceSession || g.SourceIncarnation != "inc-1" || g.Revision != 4 || !g.FilesPull || !g.FilesPush {
		t.Fatalf("migrated bound grant = %+v, %v", g, err)
	}
	if g2, err := s.SessionGrant(ctx, "g-bound2"); err != nil || g2.SourceIncarnation != "inc-2" {
		t.Fatalf("second incarnation = %+v, %v", g2, err)
	}
	// A caller that only names the session (wrong caller) gets the named
	// grant, never a bound one.
	rows, err := s.SessionGrantsFor(ctx, a, "boss")
	if ids := grantIDsOf(rows); err != nil || len(ids) != 1 || ids[0] != "g-named" {
		t.Fatalf("grants of a named caller = %v, %v", ids, err)
	}
	acc, err := s.ListAccepts(ctx)
	if err != nil || len(acc) != 1 || !*acc[0].FilesPull || !acc[0].FilesPullBound || acc[0].FilesPushBound ||
		len(acc[0].Sessions) != 1 || acc[0].Sessions[0] != "t" {
		t.Fatalf("migrated acceptance = %+v, %v", acc, err)
	}
	// Another flag or the same flag kept on keeps the marker; switching the
	// flag off and on again clears it.
	on, off := true, false
	put := func(pull, push *bool) AcceptRow {
		t.Helper()
		if err := s.PutAcceptIfOwners(ctx, AcceptRow{TargetWorkerID: b, SourceWorkerID: a, Sessions: []string{"t"}, CreatedBy: "alice",
			CreatedAt: 2, FilesPull: pull, FilesPush: push}, "alice", "alice"); err != nil {
			t.Fatal(err)
		}
		acc, _ := s.ListAccepts(ctx)
		return acc[0]
	}
	if r := put(&on, &on); !r.FilesPullBound || r.FilesPushBound {
		t.Fatalf("kept on = %+v", r)
	}
	if r := put(&off, nil); r.FilesPullBound || *r.FilesPull {
		t.Fatalf("switched off = %+v", r)
	}
	if r := put(&on, nil); r.FilesPullBound || !*r.FilesPull {
		t.Fatalf("switched on again = %+v", r)
	}
}
