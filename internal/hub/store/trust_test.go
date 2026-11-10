package store

import (
	"context"
	"errors"
	"testing"
)

// kindFixture is a store with an admin and two plain users.
type kindFixture struct {
	s             *Store
	admin, u1, u2 string
}

func newKindFixture(t *testing.T) kindFixture {
	t.Helper()
	s, admin := keyFixture(t)
	f := kindFixture{s: s, admin: admin}
	for i, dst := range []*string{&f.u1, &f.u2} {
		u := UserRow{ID: NewID(), Username: []string{"alice", "bob"}[i], PasswordHash: "x", CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1}
		if err := s.CreateUser(context.Background(), CreateUserParams{User: u, Role: "user"}); err != nil {
			t.Fatal(err)
		}
		*dst = u.ID
	}
	return f
}

func (f kindFixture) worker(t *testing.T, key byte, owner, name, kind string) WorkerRow {
	t.Helper()
	w := WorkerRow{ID: NewID(), Pub: pub(key), Name: name, OwnerID: owner, Kind: kind, CreatedAt: 1000}
	if err := f.s.InsertWorker(context.Background(), nil, w); err != nil {
		t.Fatal(err)
	}
	return w
}

func (f kindFixture) boundKey(t *testing.T, hash byte, owner, workerID string) APIKeyRow {
	t.Helper()
	k := sampleKey(owner, hash, 100)
	k.BoundWorkerID = workerID
	if err := f.s.CreateAPIKey(context.Background(), k, nil); err != nil {
		t.Fatal(err)
	}
	return k
}

func (f kindFixture) revoked(t *testing.T, id string) bool {
	t.Helper()
	k, err := f.s.APIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return k.RevokedAt != 0
}

func TestTrustRowsAreIdempotentAndScopedToTheReceiver(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutTrust(ctx, TrustRow{ReceiverID: "u1", WorkerID: "w1", CreatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	// A repeat keeps the original row.
	if err := s.PutTrust(ctx, TrustRow{ReceiverID: "u1", WorkerID: "w1", CreatedAt: 99}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTrust(ctx, TrustRow{ReceiverID: "u1", WorkerID: "w2", SessionName: "minion-1", CreatedAt: 20}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListTrust(ctx, "u1")
	want := []TrustRow{{"u1", "w1", "", 10}, {"u1", "w2", "minion-1", 20}}
	if err != nil || len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	if rows, _ := s.ListTrust(ctx, "u2"); len(rows) != 0 {
		t.Errorf("another receiver sees %+v", rows)
	}
	if err := s.PutTrust(ctx, TrustRow{WorkerID: "w1"}); err == nil {
		t.Error("a row without a receiver was stored")
	}
}

func TestHasTrustWholeWorkerCoversEverySessionAButASessionRowOnlyItself(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.PutTrust(ctx, TrustRow{ReceiverID: "u1", WorkerID: "wide", CreatedAt: 1})
	_ = s.PutTrust(ctx, TrustRow{ReceiverID: "u1", WorkerID: "narrow", SessionName: "minion-1", CreatedAt: 1})
	tests := []struct {
		worker, session string
		want            bool
	}{
		{"wide", "", true}, {"wide", "minion-7", true},
		{"narrow", "minion-1", true}, {"narrow", "minion-2", false}, {"narrow", "", false},
		{"other", "", false},
	}
	for _, tc := range tests {
		got, err := s.HasTrust(ctx, "u1", tc.worker, tc.session)
		if err != nil || got != tc.want {
			t.Errorf("HasTrust(%s, %q) = %v, %v; want %v", tc.worker, tc.session, got, err, tc.want)
		}
	}
	if ok, _ := s.HasTrust(ctx, "u2", "wide", ""); ok {
		t.Error("u2 inherits u1's trust")
	}
}

func TestDeleteTrustRemovesExactlyOneRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.PutTrust(ctx, TrustRow{ReceiverID: "u1", WorkerID: "w", CreatedAt: 1})
	_ = s.PutTrust(ctx, TrustRow{ReceiverID: "u1", WorkerID: "w", SessionName: "minion-1", CreatedAt: 1})
	if ok, err := s.DeleteTrust(ctx, "u1", "w", ""); err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteTrust(ctx, "u1", "w", ""); ok {
		t.Error("a second delete reported a row")
	}
	if ok, _ := s.HasTrust(ctx, "u1", "w", "minion-1"); !ok {
		t.Error("the session row went with the whole-Worker row")
	}
	if ok, _ := s.HasTrust(ctx, "u1", "w", "minion-2"); ok {
		t.Error("the whole-Worker row survived")
	}
}

func TestReassignWorkerIsOneTransaction(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	other := f.worker(t, 2, f.u1, "other", WorkerKindInfrastructure)
	for _, r := range []TrustRow{
		{f.u1, w.ID, "", 1}, {f.u2, w.ID, "minion-1", 1}, {f.admin, w.ID, "", 1}, // every receiver's, the admin's included
		{f.u1, other.ID, "", 1}, // another Worker stays
	} {
		if err := f.s.PutTrust(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	bound := f.boundKey(t, 1, f.u1, w.ID)
	unrelated := f.boundKey(t, 2, f.u1, other.ID)
	unbound := f.boundKey(t, 3, f.u1, "")

	res, err := f.s.ReassignWorker(ctx, ReassignParams{
		WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 5000,
		Audit: &AuditRow{ID: NewID(), TS: 5000, Actor: f.admin, Action: "worker.reassign", Outcome: "success"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PreviousOwnerID != f.u1 || res.Worker.OwnerID != f.u2 || res.TrustDeleted != 3 || res.KeysRevoked != 1 {
		t.Errorf("result = %+v", res)
	}
	got, _ := f.s.WorkerByID(ctx, w.ID)
	if got.OwnerID != f.u2 || got.Kind != WorkerKindInfrastructure {
		t.Errorf("worker = %+v", got)
	}
	for _, r := range []string{f.u1, f.u2, f.admin} {
		if ok, _ := f.s.HasTrust(ctx, r, w.ID, "minion-1"); ok {
			t.Errorf("trust of %s survived the reassignment", r)
		}
	}
	if ok, _ := f.s.HasTrust(ctx, f.u1, other.ID, ""); !ok {
		t.Error("trust of another Worker was deleted")
	}
	if !f.revoked(t, bound.ID) || f.revoked(t, unrelated.ID) || f.revoked(t, unbound.ID) {
		t.Error("only the key bound to the Worker is revoked")
	}
	rows, _ := f.s.ListAudit(ctx, AuditQuery{Action: "worker.reassign", Limit: 5})
	if len(rows) != 1 || rows[0].Target != w.ID {
		t.Errorf("audit rows = %+v", rows)
	}
}

func TestReassignWorkerRefusalsChangeNothing(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	f.worker(t, 2, f.u2, "pi", WorkerKindPrivate) // the name is taken in u2's space
	private := f.worker(t, 3, f.u1, "mine", WorkerKindPrivate)
	if err := f.s.PutTrust(ctx, TrustRow{f.u1, w.ID, "", 1}); err != nil {
		t.Fatal(err)
	}
	bound := f.boundKey(t, 1, f.u1, w.ID)
	audit := &AuditRow{ID: NewID(), TS: 1, Actor: f.admin, Action: "worker.reassign", Outcome: "success"}

	tests := []struct {
		name     string
		workerID string
		to       string
		want     error
	}{
		{"name collision", w.ID, f.u2, ErrWorkerNameTaken},
		{"private worker", private.ID, f.u2, ErrWorkerNotInfrastructure},
		{"unknown worker", NewID(), f.u2, ErrWorkerNotFound},
		{"unknown user", w.ID, NewID(), ErrNotFound},
		{"same owner", w.ID, f.u1, ErrWorkerSameOwner},
	}
	for _, tc := range tests {
		a := *audit
		a.ID = NewID()
		_, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: tc.workerID, NewOwnerID: tc.to, AtMS: 9, Audit: &a})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got, _ := f.s.WorkerByID(ctx, w.ID); got.OwnerID != f.u1 {
		t.Errorf("owner = %s after refusals", got.OwnerID)
	}
	if ok, _ := f.s.HasTrust(ctx, f.u1, w.ID, ""); !ok {
		t.Error("trust was deleted by a refused reassignment")
	}
	if f.revoked(t, bound.ID) {
		t.Error("a key was revoked by a refused reassignment")
	}
	if rows, _ := f.s.ListAudit(ctx, AuditQuery{Action: "worker.reassign", Limit: 5}); len(rows) != 0 {
		t.Errorf("a refused reassignment wrote audit rows: %+v", rows)
	}
}

func TestReassignWorkerRefusesADeactivatedOwner(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	if _, err := f.s.DeactivateUser(ctx, DeactivateUserParams{UserID: f.u2, NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 9}); !errors.Is(err, ErrUserInactive) {
		t.Errorf("err = %v, want ErrUserInactive", err)
	}
}

func TestReassignWorkerRollsBackWhenTheAuditRowFails(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	_ = f.s.PutTrust(ctx, TrustRow{f.u1, w.ID, "", 1})
	bound := f.boundKey(t, 1, f.u1, w.ID)
	dup := AuditRow{ID: NewID(), TS: 1, Actor: "a", Action: "x", Outcome: "success"}
	if err := f.s.InsertAudit(ctx, nil, dup); err != nil {
		t.Fatal(err)
	}
	// The same primary key makes the last statement of the transaction fail.
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 9, Audit: &dup}); err == nil {
		t.Fatal("reassignment succeeded with a failing audit row")
	}
	if got, _ := f.s.WorkerByID(ctx, w.ID); got.OwnerID != f.u1 {
		t.Error("the owner changed although the transaction failed")
	}
	if ok, _ := f.s.HasTrust(ctx, f.u1, w.ID, ""); !ok || f.revoked(t, bound.ID) {
		t.Error("trust or keys changed although the transaction failed")
	}
}

func TestWorkerPreviewOnlyPolicy(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindPrivate)
	if on, err := f.s.WorkerPreviewOnly(ctx, w.ID); err != nil || on {
		t.Fatalf("default policy = %v, %v", on, err)
	}
	if changed, err := f.s.SetWorkerPreviewOnly(ctx, w.ID, true); err != nil || !changed {
		t.Fatalf("set = %v, %v", changed, err)
	}
	if changed, _ := f.s.SetWorkerPreviewOnly(ctx, w.ID, true); changed {
		t.Error("setting the same value reported a change")
	}
	if on, _ := f.s.WorkerPreviewOnly(ctx, w.ID); !on {
		t.Error("policy not stored")
	}
	if changed, _ := f.s.SetWorkerPreviewOnly(ctx, w.ID, false); !changed {
		t.Error("lifting did not report a change")
	}
	if _, err := f.s.SetWorkerPreviewOnly(ctx, NewID(), true); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
	if _, err := f.s.WorkerPreviewOnly(ctx, NewID()); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
}

func TestRenameWorker(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	a := f.worker(t, 1, f.u1, "pi", WorkerKindPrivate)
	f.worker(t, 2, f.u1, "nuc", WorkerKindPrivate)
	f.worker(t, 3, f.u2, "lab", WorkerKindPrivate)
	if w, err := f.s.RenameWorker(ctx, a.ID, "lab"); err != nil || w.Name != "lab" {
		t.Fatalf("rename = %+v, %v (another owner's name is free)", w, err)
	}
	if _, err := f.s.RenameWorker(ctx, a.ID, "nuc"); !errors.Is(err, ErrWorkerNameTaken) {
		t.Errorf("collision: %v", err)
	}
	if got, _ := f.s.WorkerByID(ctx, a.ID); got.Name != "lab" {
		t.Errorf("name after a refused rename = %q", got.Name)
	}
	if _, err := f.s.RenameWorker(ctx, a.ID, "lab"); err != nil {
		t.Errorf("renaming to the current name: %v", err)
	}
	if _, err := f.s.RenameWorker(ctx, NewID(), "x"); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
}

func TestTrustTableRejectsRowsWithoutReceiverOrWorker(t *testing.T) {
	s := openTest(t)
	if _, err := s.db.Exec(`INSERT INTO trust (receiver_user_id, worker_id, session_name, created_at) VALUES ('', 'w', '', 1)`); err == nil {
		t.Error("an empty receiver was accepted")
	}
}

func TestPutTrustIfOwnerStoresOnlyUnderTheOwnerItWasDecidedFor(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	row := TrustRow{ReceiverID: f.u1, WorkerID: w.ID, CreatedAt: 5}

	if err := f.s.PutTrustIfOwner(ctx, row, f.u1); err != nil {
		t.Fatalf("under the current owner: %v", err)
	}
	if err := f.s.PutTrustIfOwner(ctx, row, f.u1); err != nil {
		t.Fatalf("a repeat: %v", err)
	}
	if rows, _ := f.s.ListTrust(ctx, f.u1); len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	// The reassignment deletes the mark; a stale decision cannot bring it back.
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutTrustIfOwner(ctx, row, f.u1); !errors.Is(err, ErrWorkerOwnerChanged) {
		t.Errorf("a stale owner: %v, want ErrWorkerOwnerChanged", err)
	}
	if rows, _ := f.s.ListTrust(ctx, f.u1); len(rows) != 0 {
		t.Errorf("trust came back: %+v", rows)
	}
	if err := f.s.PutTrustIfOwner(ctx, TrustRow{ReceiverID: f.u2, WorkerID: "nope"}, f.u2); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("unknown Worker: %v", err)
	}
	if err := f.s.PutTrustIfOwner(ctx, TrustRow{ReceiverID: f.u2, WorkerID: w.ID}, ""); err == nil {
		t.Error("accepted an empty owner")
	}
}
