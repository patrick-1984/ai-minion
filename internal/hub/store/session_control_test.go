package store

// Session control computed from minion control (v0.9.0, A17): a controller
// session reaches what the Workers that accept control from its Worker allow,
// narrowed by the exact targets of a migrated v0.8 grant.

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
)

func TestSessionControlStartsEmpty(t *testing.T) {
	s := openTest(t)
	rows, err := s.ListSessionControl(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("a fresh store lists %+v, err=%v; want no grants (default deny)", rows, err)
	}
	if _, err := s.SessionControl(context.Background(), "w1", "boss"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get on an empty store = %v, want ErrNotFound", err)
	}
}

// A v0.8-shaped grant is stored as a master session with its targets as limit
// plus acceptances on the targets; it reaches exactly its old targets.
func TestSessionControlCRUD(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := SessionControlRow{WorkerID: "w1", SessionName: "boss", TargetsJSON: `[{"kind":"session","worker_id":"w3","session_name":"build"}]`, CreatedBy: "u1", CreatedAt: 10}
	b := SessionControlRow{WorkerID: "w1", SessionName: "alpha", TargetsJSON: `[{"kind":"worker","worker_id":"w2"}]`, CreatedBy: "u1", CreatedAt: 20}
	for _, r := range []SessionControlRow{a, b} {
		if err := s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListSessionControl(ctx)
	if err != nil || len(rows) != 2 || rows[0] != b || rows[1] != a {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	if ok, err := s.DeleteSessionControl(ctx, "w1", "boss"); err != nil || !ok {
		t.Errorf("delete = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteSessionControl(ctx, "w1", "boss"); ok {
		t.Error("a second delete reported a row")
	}
	if rows, _ := s.ListSessionControl(ctx); len(rows) != 1 || rows[0] != b {
		t.Errorf("rows after delete = %+v", rows)
	}
	if err := s.PutSessionControl(ctx, SessionControlRow{WorkerID: "w1", SessionName: "x"}); err == nil {
		t.Error("a row without targets was stored")
	}
}

// Two migrated grants of one Worker keep their own targets: the acceptances
// both target Workers gave do not widen either (Codex panel P1).
func TestMigratedGrantsDoNotWidenEachOther(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, r := range []SessionControlRow{
		{WorkerID: "w1", SessionName: "s1", TargetsJSON: `[{"kind":"session","worker_id":"wB","session_name":"x"}]`, CreatedBy: "u", CreatedAt: 1},
		{WorkerID: "w1", SessionName: "s2", TargetsJSON: `[{"kind":"session","worker_id":"wC","session_name":"y"}]`, CreatedBy: "u", CreatedAt: 1},
	} {
		if err := s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	g1, _ := s.SessionControl(ctx, "w1", "s1")
	g2, _ := s.SessionControl(ctx, "w1", "s2")
	if g1.TargetsJSON != `[{"kind":"session","worker_id":"wB","session_name":"x"}]` || g2.TargetsJSON != `[{"kind":"session","worker_id":"wC","session_name":"y"}]` {
		t.Fatalf("s1 = %s, s2 = %s", g1.TargetsJSON, g2.TargetsJSON)
	}
}

// Both sides must consent: a master session reaches nothing until a Worker
// accepts its Worker; acceptance without a master reaches nothing either.
func TestMasterSessionAndAcceptanceAreBothNeeded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutAccept(ctx, AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionControl(ctx, "wA", "boss"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("acceptance alone: %v", err)
	}
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "wA", SessionName: "boss", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	g, err := s.SessionControl(ctx, "wA", "boss")
	if err != nil || g.TargetsJSON != `[{"kind":"worker","worker_id":"wB"}]` {
		t.Fatalf("master + acceptance: %+v %v", g, err)
	}
	if _, err := s.SessionControl(ctx, "wA", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a slave session reached something: %v", err)
	}
	// The direction matters: wB's masters do not reach wA.
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "wB", SessionName: "boss", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionControl(ctx, "wB", "boss"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("control flowed backwards: %v", err)
	}
	// A session limit narrows the acceptance; All Workers adds every Worker.
	if err := s.PutAccept(ctx, AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", Sessions: []string{"ci"}, CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	all := true
	if err := s.SetWorkerControl(ctx, "wC", nil, &all); err != nil {
		t.Fatal(err)
	}
	g, _ = s.SessionControl(ctx, "wA", "boss")
	if g.TargetsJSON != `[{"kind":"session","worker_id":"wB","session_name":"ci"},{"kind":"worker","worker_id":"wC"}]` {
		t.Fatalf("narrowed + all: %s", g.TargetsJSON)
	}
	// A Worker never controls itself through All Workers.
	if err := s.SetWorkerControl(ctx, "wA", nil, &all); err != nil {
		t.Fatal(err)
	}
	g, _ = s.SessionControl(ctx, "wA", "boss")
	if g.TargetsJSON != `[{"kind":"session","worker_id":"wB","session_name":"ci"},{"kind":"worker","worker_id":"wC"}]` {
		t.Fatalf("self reached: %s", g.TargetsJSON)
	}
	// A legacy grant limits; a nomination made in the screen clears the limit.
	if err := s.PutSessionControl(ctx, SessionControlRow{WorkerID: "wA", SessionName: "boss", TargetsJSON: `[{"kind":"worker","worker_id":"wC"}]`, CreatedBy: "u", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if g, _ = s.SessionControl(ctx, "wA", "boss"); g.TargetsJSON != `[{"kind":"worker","worker_id":"wC"}]` {
		t.Fatalf("legacy limit: %s", g.TargetsJSON)
	}
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "wA", SessionName: "boss", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if g, _ = s.SessionControl(ctx, "wA", "boss"); g.TargetsJSON == `[{"kind":"worker","worker_id":"wC"}]` {
		t.Fatalf("the legacy limit was not cleared: %s", g.TargetsJSON)
	}
}

func TestWorkerControlDefaults(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	c, err := s.WorkerControl(ctx, "w1")
	if err != nil || !c.UserManaged || c.AcceptAll {
		t.Fatalf("defaults = %+v %v; want user-managed, accepting nobody", c, err)
	}
	off := false
	if err := s.SetWorkerControl(ctx, "w1", &off, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.WorkerControl(ctx, "w1"); c.UserManaged || c.AcceptAll {
		t.Fatalf("after set = %+v", c)
	}
}

// A reassignment deletes the minion control of the Worker in both directions:
// its masters, and what it accepted and was accepted for.
func TestSessionControlOfAWorkerIsDeletedOnReassignment(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	other := f.worker(t, 2, f.u1, "other", WorkerKindInfrastructure)
	third := f.worker(t, 3, f.u1, "third", WorkerKindInfrastructure)
	for _, r := range []SessionControlRow{
		{WorkerID: w.ID, SessionName: "boss", TargetsJSON: `[{"kind":"worker","worker_id":"` + other.ID + `"}]`, CreatedBy: f.admin, CreatedAt: 1},
		{WorkerID: other.ID, SessionName: "boss", TargetsJSON: `[{"kind":"worker","worker_id":"` + w.ID + `"},{"kind":"worker","worker_id":"` + third.ID + `"}]`, CreatedBy: f.admin, CreatedAt: 1},
	} {
		if err := f.s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 5000}); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.s.ListSessionControl(ctx)
	if len(rows) != 1 || rows[0].WorkerID != other.ID || rows[0].TargetsJSON != `[{"kind":"worker","worker_id":"`+third.ID+`"}]` {
		t.Errorf("rows after reassignment = %+v, want only other -> third", rows)
	}
}

func TestSessionControlSurvivesARefusedReassignment(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	f.worker(t, 2, f.u1, "other", WorkerKindInfrastructure)
	if err := f.s.PutSessionControl(ctx, SessionControlRow{WorkerID: w.ID, SessionName: "boss", TargetsJSON: `[{"kind":"everything"}]`, CreatedBy: f.admin, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u1, AtMS: 5000}); !errors.Is(err, ErrWorkerSameOwner) {
		t.Fatalf("err = %v", err)
	}
	if rows, _ := f.s.ListSessionControl(ctx); len(rows) != 1 {
		t.Errorf("a refused reassignment changed the grants: %+v", rows)
	}
}

func TestDeleteSessionControlOfWorker(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, r := range []SessionControlRow{
		{WorkerID: "w1", SessionName: "a", TargetsJSON: `[{"kind":"worker","worker_id":"w3"}]`, CreatedBy: "u", CreatedAt: 1},
		{WorkerID: "w1", SessionName: "b", TargetsJSON: `[{"kind":"worker","worker_id":"w3"}]`, CreatedBy: "u", CreatedAt: 1},
		{WorkerID: "w2", SessionName: "a", TargetsJSON: `[{"kind":"worker","worker_id":"w3"}]`, CreatedBy: "u", CreatedAt: 1},
	} {
		if err := s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// w1's two masters and the acceptance w3 gave w1.
	if n, err := s.DeleteSessionControlOfWorker(ctx, nil, "w1"); err != nil || n != 3 {
		t.Errorf("deleted %d, err=%v; want 3", n, err)
	}
	if rows, _ := s.ListSessionControl(ctx); len(rows) != 1 || rows[0].WorkerID != "w2" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestPutSessionControlIfOwnerStoresOnlyUnderTheOwnerItWasDecidedFor(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	f.worker(t, 2, f.u1, "other", WorkerKindInfrastructure)
	row := SessionControlRow{WorkerID: w.ID, SessionName: "boss", TargetsJSON: `[{"kind":"everything"}]`, CreatedBy: f.admin, CreatedAt: 5}
	if err := f.s.PutSessionControlIfOwner(ctx, row, f.u1); err != nil {
		t.Fatalf("under the current owner: %v", err)
	}
	// The reassignment deletes the grant; a stale decision cannot bring it back.
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutSessionControlIfOwner(ctx, row, f.u1); !errors.Is(err, ErrWorkerOwnerChanged) {
		t.Errorf("a stale owner: %v, want ErrWorkerOwnerChanged", err)
	}
	// A removed Worker cannot get a grant back either.
	if err := f.s.TombstoneWorker(ctx, w.ID, f.admin, 2); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutSessionControlIfOwner(ctx, row, f.u2); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("a removed Worker: %v, want ErrWorkerNotFound", err)
	}
	if rows, _ := f.s.ListSessionControl(ctx); len(rows) != 0 {
		t.Errorf("a grant came back: %+v", rows)
	}
	if err := f.s.PutSessionControlIfOwner(ctx, row, ""); err == nil {
		t.Error("accepted an empty owner")
	}
}

// Changing one switch never rewrites the other (two autosaves in a row).
func TestSetWorkerControlChangesOnlyTheGivenSwitch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	off, on := false, true
	if err := s.SetWorkerControl(ctx, "w1", &off, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkerControl(ctx, "w1", nil, &on); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.WorkerControl(ctx, "w1"); c.UserManaged || !c.AcceptAll {
		t.Fatalf("= %+v, want user-managed off and accept-all on", c)
	}
}

// A migrated v0.8 grant that reached its own Worker keeps reaching it (within
// its limit); a nomination made in the screen never reaches its own Worker.
func TestMigratedGrantsKeepTheirOwnWorker(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, r := range []SessionControlRow{
		{WorkerID: "w1", SessionName: "all", TargetsJSON: `[{"kind":"everything"}]`, CreatedBy: "u", CreatedAt: 1},
		{WorkerID: "w1", SessionName: "self", TargetsJSON: `[{"kind":"session","worker_id":"w1","session_name":"ci"}]`, CreatedBy: "u", CreatedAt: 1},
	} {
		if err := s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if g, err := s.SessionControl(ctx, "w1", "all"); err != nil || g.TargetsJSON != `[{"kind":"worker","worker_id":"w1"}]` {
		t.Fatalf("everything on a one-Worker Hub: %+v %v", g, err)
	}
	if g, err := s.SessionControl(ctx, "w1", "self"); err != nil || g.TargetsJSON != `[{"kind":"session","worker_id":"w1","session_name":"ci"}]` {
		t.Fatalf("own session: %+v %v", g, err)
	}
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "w1", SessionName: "all", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionControl(ctx, "w1", "all"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a new nomination reached its own Worker: %v", err)
	}
}

// A removal decided for an earlier owner does not delete what was set up
// for the new owner.
func TestStaleRemovalsKeepTheNewOwnersRows(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	a := f.worker(t, 1, f.u1, "a", WorkerKindInfrastructure)
	b := f.worker(t, 2, f.u1, "b", WorkerKindInfrastructure)
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: a.ID, NewOwnerID: f.u2, AtMS: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutMasterSessionIfOwner(ctx, MasterRow{WorkerID: a.ID, SessionName: "boss", CreatedBy: f.admin}, f.u2); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutAcceptIfOwners(ctx, AcceptRow{TargetWorkerID: b.ID, SourceWorkerID: a.ID, CreatedBy: f.admin}, f.u1, f.u2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DeleteMasterSessionIfOwner(ctx, a.ID, "boss", f.u1); !errors.Is(err, ErrWorkerOwnerChanged) {
		t.Errorf("stale master removal: %v", err)
	}
	if _, err := f.s.DeleteAcceptIfOwners(ctx, b.ID, a.ID, f.u1, f.u1); !errors.Is(err, ErrWorkerOwnerChanged) {
		t.Errorf("stale acceptance removal: %v", err)
	}
	if ms, _ := f.s.ListMasterSessions(ctx); len(ms) != 1 {
		t.Errorf("masters = %+v", ms)
	}
	if as, _ := f.s.ListAccepts(ctx); len(as) != 1 {
		t.Errorf("acceptances = %+v", as)
	}
	if ok, err := f.s.DeleteAcceptIfOwners(ctx, b.ID, a.ID, f.u1, f.u2); err != nil || !ok {
		t.Errorf("current owners: %v %v", ok, err)
	}
}

// A20: a session that ends loses its nomination and its place in limited
// acceptances; when the name comes back it is a slave session again.
func TestGoneSessionsLoseTheirControlSettings(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, m := range []MasterRow{{WorkerID: "wA", SessionName: "boss", CreatedBy: "u"}, {WorkerID: "wA", SessionName: "keep", CreatedBy: "u"}} {
		if err := s.PutMasterSession(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []AcceptRow{
		{TargetWorkerID: "wA", SourceWorkerID: "wB", Sessions: []string{"boss", "ci"}, CreatedBy: "u"},
		{TargetWorkerID: "wA", SourceWorkerID: "wC", Sessions: []string{"boss"}, CreatedBy: "u"},
		{TargetWorkerID: "wA", SourceWorkerID: "wD", CreatedBy: "u"},
	} {
		if err := s.PutAccept(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PurgeGoneSessions(ctx, "wA", []string{"boss"}, math.MaxInt64); err != nil || n != 3 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	ms, _ := s.ListMasterSessions(ctx)
	if len(ms) != 1 || ms[0].SessionName != "keep" {
		t.Errorf("masters = %+v", ms)
	}
	// wC named only boss: its acceptance stays as an empty list (nothing),
	// never NULL (the whole Worker).
	as, _ := s.ListAccepts(ctx)
	if len(as) != 3 || as[0].SourceWorkerID != "wB" || len(as[0].Sessions) != 1 || as[0].Sessions[0] != "ci" ||
		as[1].SourceWorkerID != "wC" || as[1].Sessions == nil || len(as[1].Sessions) != 0 || as[2].SourceWorkerID != "wD" || as[2].Sessions != nil {
		t.Errorf("acceptances = %+v", as)
	}
	// After a resync only "ci" is there: "keep" goes too.
	if _, err := s.PurgeSessionsNotIn(ctx, "wA", []string{"ci"}, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.ListMasterSessions(ctx); len(ms) != 0 {
		t.Errorf("masters after resync = %+v", ms)
	}
}

// A purge never widens: with "accepts all Workers" on, an acceptance limited
// to a session that ends allows nothing, not the whole Worker.
func TestPurgeNeverFallsBackToAcceptAll(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	all := true
	if err := s.SetWorkerControl(ctx, "wB", nil, &all); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccept(ctx, AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", Sessions: []string{"ci"}, CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "wA", SessionName: "boss", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeGoneSessions(ctx, "wB", []string{"ci"}, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if g, err := s.SessionControl(ctx, "wA", "boss"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant after the purge = %+v, %v; want nothing", g, err)
	}
}

// Revoking a source on a Worker that accepts all must not delete the row: the
// source would fall back to accept_all and reach every shared session.
func TestRevokeAcceptNeverFallsBackToAcceptAll(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// The owner fence needs real Workers.
	oA, oB := "alice", "bob"
	for _, w := range []WorkerRow{worker("wA", 4, oA, "a"), worker("wB", 5, oB, "b")} {
		if err := s.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	all := true
	if err := s.SetWorkerControl(ctx, "wB", nil, &all); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccept(ctx, AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", Sessions: []string{"ci"}, CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "wA", SessionName: "boss", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionControl(ctx, "wA", "boss"); err != nil {
		t.Fatalf("before: %v", err)
	}
	revoke := AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", CreatedBy: "u", CreatedAt: 5}
	if ok, err := s.RevokeAcceptIfOwners(ctx, revoke, oB, oA); err != nil || !ok {
		t.Fatalf("revoke = %t, %v", ok, err)
	}
	if g, err := s.SessionControl(ctx, "wA", "boss"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant after the revoke = %+v, %v; want nothing", g, err)
	}
	var sess sql.NullString
	if err := s.DB().QueryRowContext(ctx, `SELECT sessions_json FROM control_accept WHERE target_worker_id='wB' AND source_worker_id='wA'`).Scan(&sess); err != nil || sess.String != "[]" {
		t.Fatalf("row after the revoke = %+v, %v; want []", sess, err)
	}
	// A source without a row is blocked the same way.
	if _, err := s.DeleteAccept(ctx, "wB", "wA"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeAcceptIfOwners(ctx, revoke, oB, oA); err != nil {
		t.Fatal(err)
	}
	if g, err := s.SessionControl(ctx, "wA", "boss"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant after the revoke without a row = %+v, %v; want nothing", g, err)
	}
	// Without accept_all the row is deleted as before.
	off := false
	if err := s.SetWorkerControl(ctx, "wB", nil, &off); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccept(ctx, AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", Sessions: []string{"ci"}, CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RevokeAcceptIfOwners(ctx, revoke, oB, oA); err != nil || !ok {
		t.Fatalf("revoke without accept_all = %t, %v", ok, err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM control_accept WHERE target_worker_id='wB' AND source_worker_id='wA'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after the revoke = %d, %v; want 0", n, err)
	}
	if ok, err := s.RevokeAcceptIfOwners(ctx, revoke, "someone-else", oA); !errors.Is(err, ErrWorkerOwnerChanged) || ok {
		t.Fatalf("revoke for another owner = %t, %v", ok, err)
	}
}

// Migrated (legacy) limits lose gone sessions too; a limit left empty allows
// nothing.
func TestPurgeCleansLegacyTargets(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutSessionControl(ctx, SessionControlRow{WorkerID: "wA", SessionName: "boss",
		TargetsJSON: `[{"kind":"session","worker_id":"wB","session_name":"ci"}]`, CreatedBy: "u", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionControl(ctx, "wA", "boss"); err != nil {
		t.Fatalf("before: %v", err)
	}
	if n, err := s.PurgeGoneSessions(ctx, "wB", []string{"ci"}, math.MaxInt64); err != nil || n == 0 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if _, err := s.SessionControl(ctx, "wA", "boss"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a new ci on wB is still a target: %v", err)
	}
	m, _ := s.MasterSession(ctx, "wA", "boss")
	if m.LegacyTargetsJSON != "[]" {
		t.Errorf("legacy limit = %q", m.LegacyTargetsJSON)
	}
}

// A purge acts on what was saved before its evidence: a nomination made after
// the session came back (a later click) stays.
func TestPurgeKeepsSettingsSavedAfterTheEvidence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "wA", SessionName: "ci", CreatedBy: "u", CreatedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccept(ctx, AcceptRow{TargetWorkerID: "wA", SourceWorkerID: "wB", Sessions: []string{"ci"}, CreatedBy: "u", CreatedAt: 200}); err != nil {
		t.Fatal(err)
	}
	// The snapshot without ci was applied at 100, before both were saved.
	if n, err := s.PurgeSessionsNotIn(ctx, "wA", nil, 100); err != nil || n != 0 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if n, err := s.PurgeGoneSessions(ctx, "wA", []string{"ci"}, 150); err != nil || n != 0 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if ms, _ := s.ListMasterSessions(ctx); len(ms) != 1 {
		t.Errorf("masters = %+v", ms)
	}
	if as, _ := s.ListAccepts(ctx); len(as) != 1 || len(as[0].Sessions) != 1 {
		t.Errorf("acceptances = %+v", as)
	}
}

// A save in the same millisecond as the evidence counts as later.
func TestPurgeKeepsASaveInTheSameMillisecond(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutMasterSession(ctx, MasterRow{WorkerID: "wA", SessionName: "ci", CreatedBy: "u", CreatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PurgeGoneSessions(ctx, "wA", []string{"ci"}, 100); n != 0 {
		t.Fatalf("a same-millisecond save was purged")
	}
	if n, _ := s.PurgeGoneSessions(ctx, "wA", []string{"ci"}, 101); n != 1 {
		t.Fatalf("an older save was kept")
	}
}

// Content of a Worker that does not exist (removal in progress) is never user-managed,
// whatever a control row or the missing-row default says.
func TestWorkerContentManagedNeedsTheWorker(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	on := true
	_ = s.SetWorkerControl(ctx, "gone", &on, nil)
	for _, id := range []string{"gone", "never-existed"} {
		if ok, err := s.WorkerContentManaged(ctx, id); ok || err != nil {
			t.Fatalf("%s = %v, %v", id, ok, err)
		}
	}
}

// A name-only revoke of a slot held by an active session-bound master is
// refused inside the delete's transaction, on both delete paths.
func TestNameOnlyDeleteRefusesAnActiveBoundSlotInTheTransaction(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	w := worker("wA", 4, "alice", "a")
	if err := s.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO bound_master_session
		(worker_id,owner_id,session_name,incarnation_id,worker_pub,generation,active,legacy_targets_json,changed_at)
		VALUES (?,?,?,?,?,1,1,'',1)`, w.ID, w.OwnerID, "boss", "inc-1", w.Pub); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteMasterSession(ctx, w.ID, "boss"); !errors.Is(err, ErrBoundMasterSlot) {
		t.Fatalf("DeleteMasterSession = %v, want ErrBoundMasterSlot", err)
	}
	if _, err := s.DeleteMasterSessionIfOwner(ctx, w.ID, "boss", w.OwnerID); !errors.Is(err, ErrBoundMasterSlot) {
		t.Fatalf("DeleteMasterSessionIfOwner = %v, want ErrBoundMasterSlot", err)
	}
	if _, err := s.DeleteMasterSession(ctx, w.ID, "other"); err != nil {
		t.Fatalf("a plain name is deleted as before: %v", err)
	}
}
