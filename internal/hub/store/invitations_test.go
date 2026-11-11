package store

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

// openInviteTest opens a store with the active users that the tests use as
// invitation creators: a key whose creator is not an active user enrolls
// nothing.
func openInviteTest(t *testing.T) *Store {
	t.Helper()
	s := openTest(t)
	for _, id := range []string{"owner", "owner-a", "owner-b", "u1", "u2"} {
		seedActiveUser(t, s, id)
	}
	return s
}

// seedActiveUser inserts an active user with the given id (and username).
func seedActiveUser(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO users (id, username, active, created_at, updated_at) VALUES (?, ?, 1, 1, 1)`, id, id); err != nil {
		t.Fatal(err)
	}
}

func inviteRow(creator string, mode string, expires int64, secret byte) InvitationRow {
	r := InvitationRow{
		ID: NewID(), SecretHash: bytes.Repeat([]byte{secret}, 32), Name: "n", Mode: mode, Kind: WorkerKindPrivate, TTL: "1h",
		CreatorID: creator, CreatedAt: 1000, ExpiresAt: expires,
	}
	if mode == InvitationOneTime {
		r.MaxUses = 1
	}
	return r
}

func enrollParams(r InvitationRow, key byte, name string, now int64) EnrollParams {
	return EnrollParams{SecretHash: r.SecretHash, Pub: pub(key), Name: name, WorkerID: NewID(), Now: now}
}

func TestInvitationRoundTripAndList(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	a := inviteRow("u1", InvitationOneTime, 5000, 1)
	b := inviteRow("u1", InvitationReusable, 0, 2)
	b.CreatedAt = 2000
	b.Kind = WorkerKindInfrastructure
	c := inviteRow("u2", InvitationReusable, 0, 3)
	c.CreatedAt = 3000
	for _, r := range []InvitationRow{a, b, c} {
		if err := s.InsertInvitation(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.InvitationByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := a
	if got.ID != want.ID || !bytes.Equal(got.SecretHash, want.SecretHash) || got.Mode != InvitationOneTime || got.MaxUses != 1 ||
		got.ExpiresAt != 5000 || got.Uses != 0 || got.Revoked || got.RevokedAt != 0 || got.CreatorID != "u1" {
		t.Errorf("got %+v", got)
	}
	if got, _ := s.InvitationByID(ctx, b.ID); got.ExpiresAt != 0 || got.MaxUses != 0 || got.Kind != WorkerKindInfrastructure {
		t.Errorf("unlimited reusable = %+v", got)
	}
	if _, err := s.InvitationByID(ctx, "nope"); !errors.Is(err, ErrInvitationNotFound) {
		t.Errorf("unknown id: %v", err)
	}

	mine, err := s.ListInvitations(ctx, "u1")
	if err != nil || len(mine) != 2 || mine[0].ID != b.ID || mine[1].ID != a.ID {
		t.Fatalf("u1 list = %+v err=%v", mine, err)
	}
	all, err := s.ListInvitations(ctx, "")
	if err != nil || len(all) != 3 || all[0].ID != c.ID {
		t.Fatalf("all = %+v err=%v", all, err)
	}
	if none, err := s.ListInvitations(ctx, "u3"); err != nil || len(none) != 0 {
		t.Errorf("u3 list = %+v err=%v", none, err)
	}
	// The same secret hash cannot be stored twice.
	dup := inviteRow("u1", InvitationOneTime, 0, 1)
	if err := s.InsertInvitation(ctx, dup); err == nil {
		t.Error("a duplicate secret hash was accepted")
	}
}

func TestRevokeInvitationIsIdempotent(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("u1", InvitationReusable, 0, 1)
	if err := s.InsertInvitation(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvitation(ctx, r.ID, 111); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvitation(ctx, r.ID, 222); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.InvitationByID(ctx, r.ID); !got.Revoked || got.RevokedAt != 111 {
		t.Errorf("got %+v, want revoked at the first time", got)
	}
	if err := s.RevokeInvitation(ctx, "nope", 1); !errors.Is(err, ErrInvitationNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestEnrollWithInvitationChecksInOrder(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	live := inviteRow("owner", InvitationReusable, 0, 1)
	timed := inviteRow("owner", InvitationReusable, 5000, 2)
	gone := inviteRow("owner", InvitationReusable, 0, 3)
	for _, r := range []InvitationRow{live, timed, gone} {
		if err := s.InsertInvitation(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RevokeInvitation(ctx, gone.ID, 1); err != nil {
		t.Fatal(err)
	}

	if _, err := s.EnrollWithInvitation(ctx, EnrollParams{SecretHash: bytes.Repeat([]byte{9}, 32), Pub: pub(1), Name: "x", WorkerID: NewID(), Now: 1}); !errors.Is(err, ErrInvitationNotFound) {
		t.Errorf("unknown secret: %v", err)
	}
	if _, err := s.EnrollWithInvitation(ctx, enrollParams(gone, 1, "x", 1)); !errors.Is(err, ErrInvitationRevoked) {
		t.Errorf("revoked: %v", err)
	}
	if _, err := s.EnrollWithInvitation(ctx, enrollParams(timed, 1, "x", 5000)); !errors.Is(err, ErrInvitationExpired) {
		t.Errorf("expired at the deadline: %v", err)
	}
	res, err := s.EnrollWithInvitation(ctx, enrollParams(timed, 1, "x", 4999))
	if err != nil || res.Existing || res.InvitationID != timed.ID || res.Worker.OwnerID != "owner" {
		t.Fatalf("before the deadline: %+v %v", res, err)
	}
	// A refusal still reports which key it was about.
	if res, err := s.EnrollWithInvitation(ctx, enrollParams(gone, 2, "y", 1)); err == nil || res.InvitationID != gone.ID {
		t.Errorf("refusal: %+v %v", res, err)
	}
}

func TestEnrollConsumesOneUseAndIsIdempotentPerKey(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("owner", InvitationOneTime, 0, 1)
	if err := s.InsertInvitation(ctx, r); err != nil {
		t.Fatal(err)
	}
	first, err := s.EnrollWithInvitation(ctx, enrollParams(r, 1, "pi", 10))
	if err != nil || first.Existing || first.Worker.Name != "pi" || first.Worker.Kind != WorkerKindPrivate || first.Worker.CreatedAt != 10 {
		t.Fatalf("first = %+v %v", first, err)
	}
	// The same key again: the existing row, no extra use, even though the
	// one-time key is spent.
	again, err := s.EnrollWithInvitation(ctx, enrollParams(r, 1, "renamed", 20))
	if err != nil || !again.Existing || again.Worker.ID != first.Worker.ID || again.Worker.Name != "pi" {
		t.Fatalf("again = %+v %v", again, err)
	}
	if got, _ := s.InvitationByID(ctx, r.ID); got.Uses != 1 {
		t.Errorf("uses = %d, want 1", got.Uses)
	}
	if _, err := s.EnrollWithInvitation(ctx, enrollParams(r, 2, "pi", 30)); !errors.Is(err, ErrInvitationExhausted) {
		t.Errorf("second key on a spent one-time key: %v", err)
	}
	if got, _ := s.InvitationByID(ctx, r.ID); got.Uses != 1 {
		t.Errorf("uses = %d after a refusal, want 1", got.Uses)
	}
}

func TestRacingEnrollmentsOnAOneTimeKeyAdmitExactlyOne(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("owner", InvitationOneTime, 0, 1)
	if err := s.InsertInvitation(ctx, r); err != nil {
		t.Fatal(err)
	}
	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.EnrollWithInvitation(ctx, enrollParams(r, byte(i+1), "racer", 10))
		}()
	}
	close(start)
	wg.Wait()
	var ok, exhausted int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInvitationExhausted):
			exhausted++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || exhausted != racers-1 {
		t.Fatalf("enrolled %d, exhausted %d, want 1 and %d", ok, exhausted, racers-1)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM workers`).Scan(&n); err != nil || n != 1 {
		t.Errorf("workers = %d err=%v", n, err)
	}
}

func TestEnrollNamesGetSuffixesPerOwner(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	a := inviteRow("owner-a", InvitationReusable, 0, 1)
	b := inviteRow("owner-b", InvitationReusable, 0, 2)
	for _, r := range []InvitationRow{a, b} {
		if err := s.InsertInvitation(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// A name that already looks like a suffixed one is skipped too.
	if err := s.InsertWorker(ctx, nil, worker(NewID(), 100, "owner-a", "pi-3")); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range 4 {
		res, err := s.EnrollWithInvitation(ctx, enrollParams(a, byte(i+1), "pi", 1))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, res.Worker.Name)
	}
	want := []string{"pi", "pi-2", "pi-4", "pi-5"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
	res, err := s.EnrollWithInvitation(ctx, enrollParams(b, 50, "pi", 1))
	if err != nil || res.Worker.Name != "pi" {
		t.Errorf("other owner: %+v %v", res, err)
	}
	// The stored names are what was returned.
	got, _ := s.WorkerByID(ctx, res.Worker.ID)
	if got.Name != "pi" || got.OwnerID != "owner-b" {
		t.Errorf("stored = %+v", got)
	}
}

func TestEnrollRefusesATombstonedKeyWithoutConsumingAUse(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("owner", InvitationOneTime, 0, 1)
	if err := s.InsertInvitation(ctx, r); err != nil {
		t.Fatal(err)
	}
	w := worker(NewID(), 7, "owner", "old")
	if err := s.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	if err := s.TombstoneWorker(ctx, w.ID, "owner", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollWithInvitation(ctx, enrollParams(r, 7, "old", 6)); !errors.Is(err, ErrWorkerTombstoned) {
		t.Fatalf("err = %v", err)
	}
	ir, err := s.EnsureInfraKey(ctx, bytes.Repeat([]byte{9}, 32), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollWithInvitation(ctx, EnrollParams{SecretHash: ir.SecretHash, Infra: true, OwnerID: "owner", Pub: pub(7), Name: "old", WorkerID: NewID(), Now: 6}); !errors.Is(err, ErrWorkerTombstoned) {
		t.Fatalf("infra err = %v", err)
	}
	if got, _ := s.InvitationByID(ctx, r.ID); got.Uses != 0 {
		t.Errorf("uses = %d", got.Uses)
	}
}

func TestEnrollmentAuditCommitsWithTheEnrollment(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("owner", InvitationReusable, 0, 1)
	if err := s.InsertInvitation(ctx, r); err != nil {
		t.Fatal(err)
	}
	row := func() *AuditRow {
		return &AuditRow{ID: NewID(), TS: 1, Actor: "worker:x", Action: "worker.enroll", Outcome: "success"}
	}
	p := enrollParams(r, 1, "pi", 1)
	p.Audit = row()
	res, err := s.EnrollWithInvitation(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'worker.enroll' AND target = ?`, res.Worker.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatal("no audit row for the enrollment")
	}
	// The idempotent repeat writes no second row.
	p = enrollParams(r, 1, "pi", 2)
	p.Audit = row()
	if _, err := s.EnrollWithInvitation(ctx, p); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Error("a repeated enrollment was audited again")
	}
	// A failing audit insert (duplicate id) rolls the whole enrollment back.
	dup := row()
	p = enrollParams(r, 2, "pi", 3)
	p.Audit = dup
	if _, err := s.EnrollWithInvitation(ctx, p); err != nil {
		t.Fatal(err)
	}
	before, _ := s.InvitationByID(ctx, r.ID)
	p = enrollParams(r, 3, "pi", 4)
	p.Audit = dup
	if _, err := s.EnrollWithInvitation(ctx, p); err == nil {
		t.Fatal("a duplicate audit id was accepted")
	}
	after, _ := s.InvitationByID(ctx, r.ID)
	if after.Uses != before.Uses {
		t.Errorf("uses went from %d to %d in a failed transaction", before.Uses, after.Uses)
	}
	if _, err := s.WorkerByPub(ctx, pub(3)); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("the Worker survived the rollback: %v", err)
	}
}

func TestEnrollWithTheInfrastructureKeyCreatesPendingInfrastructureWorkers(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	hash := bytes.Repeat([]byte{4}, 32)
	row, err := s.EnsureInfraKey(ctx, hash, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := EnrollParams{SecretHash: hash, Infra: true, OwnerID: "owner", Pub: pub(1), Name: "edge", WorkerID: NewID(), Now: 9, Addr: "192.0.2.7"}
	res, err := s.EnrollWithInvitation(ctx, p)
	if err != nil || res.Existing || res.InvitationID != row.ID || res.Worker.Kind != WorkerKindInfrastructure || res.Worker.OwnerID != "owner" {
		t.Fatalf("res = %+v err=%v", res, err)
	}
	if !res.Worker.Pending || res.Worker.EnrollAddr != "192.0.2.7" {
		t.Errorf("the new Worker must be pending with its address: %+v", res.Worker)
	}
	p.Pub, p.WorkerID = pub(2), NewID()
	if res, err := s.EnrollWithInvitation(ctx, p); err != nil || res.Worker.Name != "edge-2" {
		t.Errorf("second = %+v err=%v", res, err)
	}
	p.Pub, p.WorkerID = pub(1), NewID()
	if res, err := s.EnrollWithInvitation(ctx, p); err != nil || !res.Existing || res.Worker.Name != "edge" {
		t.Errorf("repeat = %+v err=%v", res, err)
	}
	if got, _ := s.InvitationByID(ctx, row.ID); got.Uses != 2 {
		t.Errorf("uses = %d, want 2 (the repeat consumes none)", got.Uses)
	}
}

func TestInfrastructureKeyRowSurvivesReloadsAndRevocationIsSticky(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	hash := bytes.Repeat([]byte{5}, 32)
	first, err := s.EnsureInfraKey(ctx, hash, 50_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExpiresAt != 50_000 || first.Source != InvitationSourceInfraFile || first.Mode != InvitationReusable {
		t.Fatalf("first = %+v", first)
	}
	p := EnrollParams{SecretHash: hash, Infra: true, OwnerID: "owner", Pub: pub(1), Name: "edge", WorkerID: NewID(), Now: 2_000}
	if _, err := s.EnrollWithInvitation(ctx, p); err != nil {
		t.Fatal(err)
	}
	// A reload (another start) with a later time and another expiry changes nothing.
	again, err := s.EnsureInfraKey(ctx, hash, 999_999, 40_000)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.ExpiresAt != 50_000 || again.Uses != 1 || again.CreatedAt != 1_000 {
		t.Errorf("a reload changed the row: %+v", again)
	}
	// Revocation sticks across reloads.
	if err := s.RevokeInvitation(ctx, first.ID, 41_000); err != nil {
		t.Fatal(err)
	}
	again, err = s.EnsureInfraKey(ctx, hash, 999_999, 42_000)
	if err != nil || !again.Revoked || again.RevokedAt != 41_000 {
		t.Fatalf("revocation was undone: %+v err=%v", again, err)
	}
	p.Pub, p.WorkerID = pub(2), NewID()
	if _, err := s.EnrollWithInvitation(ctx, p); !errors.Is(err, ErrInvitationRevoked) {
		t.Errorf("revoked infra key: %v", err)
	}
	// Expiry is enforced.
	h2 := bytes.Repeat([]byte{6}, 32)
	if _, err := s.EnsureInfraKey(ctx, h2, 3_000, 1_000); err != nil {
		t.Fatal(err)
	}
	p = EnrollParams{SecretHash: h2, Infra: true, OwnerID: "owner", Pub: pub(3), Name: "edge", WorkerID: NewID(), Now: 3_000}
	if _, err := s.EnrollWithInvitation(ctx, p); !errors.Is(err, ErrInvitationExpired) {
		t.Errorf("expired infra key: %v", err)
	}
}

func TestInfrastructureRowOnlyEnrollsWhileTheFileHoldsItsSecret(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	hash := bytes.Repeat([]byte{7}, 32)
	if _, err := s.EnsureInfraKey(ctx, hash, 0, 1); err != nil {
		t.Fatal(err)
	}
	// Presented as an ordinary invitation secret (the file no longer holds it):
	p := EnrollParams{SecretHash: hash, Pub: pub(1), Name: "x", WorkerID: NewID(), Now: 5}
	if _, err := s.EnrollWithInvitation(ctx, p); !errors.Is(err, ErrInvitationNotFound) {
		t.Errorf("a withdrawn infra key enrolled: %v", err)
	}
	// And a user invitation is not accepted as the key file.
	u := inviteRow("owner", InvitationReusable, 0, 8)
	if err := s.InsertInvitation(ctx, u); err != nil {
		t.Fatal(err)
	}
	p = EnrollParams{SecretHash: u.SecretHash, Infra: true, OwnerID: "owner", Pub: pub(2), Name: "x", WorkerID: NewID(), Now: 5}
	if _, err := s.EnrollWithInvitation(ctx, p); !errors.Is(err, ErrInvitationNotFound) {
		t.Errorf("a user invitation enrolled as the key file: %v", err)
	}
}

func TestEnrollmentCreatesPendingWorkersAndApprovalIsAudited(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("owner", InvitationReusable, 0, 1)
	if err := s.InsertInvitation(ctx, r); err != nil {
		t.Fatal(err)
	}
	p := enrollParams(r, 1, "pi", 10)
	p.Addr = "203.0.113.9"
	res, err := s.EnrollWithInvitation(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkerByID(ctx, res.Worker.ID)
	if err != nil || !w.Pending || w.EnrollAddr != "203.0.113.9" || w.ApprovedAt != 0 {
		t.Fatalf("worker = %+v err=%v", w, err)
	}
	a := &AuditRow{ID: NewID(), TS: 20, Actor: "owner", Action: "worker.approve", Outcome: "success"}
	got, err := s.ApproveWorker(ctx, w.ID, "owner", 20, a)
	if err != nil || got.Pending || got.ApprovedBy != "owner" || got.ApprovedAt != 20 {
		t.Fatalf("approved = %+v err=%v", got, err)
	}
	w, _ = s.WorkerByID(ctx, w.ID)
	if w.Pending {
		t.Error("the Worker is still pending")
	}
	// A second approval changes nothing and writes no second audit row.
	a2 := &AuditRow{ID: NewID(), TS: 21, Actor: "owner", Action: "worker.approve", Outcome: "success"}
	if _, err := s.ApproveWorker(ctx, w.ID, "owner", 21, a2); !errors.Is(err, ErrWorkerAlreadyApproved) {
		t.Errorf("second approval: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'worker.approve' AND target = ?`, w.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("approve audit rows = %d err=%v", n, err)
	}
	if _, err := s.ApproveWorker(ctx, NewID(), "owner", 22, nil); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
}

func TestInfraOwnerID(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.InfraOwnerID(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("before the seed: %v", err)
	}
	p := seedParams("root")
	p.Marker = AdminSeedMarker
	if created, err := s.SeedAdmin(ctx, p); err != nil || !created {
		t.Fatalf("seed: %v %v", created, err)
	}
	if id, err := s.InfraOwnerID(ctx, ""); err != nil || id != p.User.ID {
		t.Errorf("seed admin = %q err=%v, want %q", id, err, p.User.ID)
	}
	if id, err := s.InfraOwnerID(ctx, "ROOT"); err != nil || id != p.User.ID {
		t.Errorf("by name (case-insensitive) = %q err=%v", id, err)
	}
	if _, err := s.InfraOwnerID(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown name: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, p.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "root"} {
		if _, err := s.InfraOwnerID(ctx, name); !errors.Is(err, ErrNotFound) {
			t.Errorf("deactivated user %q: %v", name, err)
		}
	}
}

func TestEnrollRefusesAKeyWhoseCreatorIsNotActive(t *testing.T) {
	s := openInviteTest(t)
	ctx := context.Background()
	r := inviteRow("owner", InvitationReusable, 0, 1)
	ghost := inviteRow("nobody", InvitationReusable, 0, 2)
	for _, x := range []InvitationRow{r, ghost} {
		if err := s.InsertInvitation(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.EnrollWithInvitation(ctx, enrollParams(r, 1, "pi", 10))
	if err != nil || first.Existing {
		t.Fatalf("enroll while active: %+v %v", first, err)
	}
	if _, err := s.db.Exec(`UPDATE users SET active = 0 WHERE id = 'owner'`); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]EnrollParams{
		"deactivated creator": enrollParams(r, 2, "pi", 10),
		"unknown creator":     enrollParams(ghost, 3, "pi", 10),
	} {
		res, err := s.EnrollWithInvitation(ctx, p)
		if !errors.Is(err, ErrInvitationRevoked) {
			t.Errorf("%s: err = %v, want ErrInvitationRevoked", name, err)
		}
		if res.InvitationID == "" {
			t.Errorf("%s: the refusal does not name the invitation", name)
		}
	}
	for _, x := range []InvitationRow{r, ghost} {
		got, _ := s.InvitationByID(ctx, x.ID)
		want := 0
		if x.ID == r.ID {
			want = 1
		}
		if got.Uses != want {
			t.Errorf("uses of %s = %d, want %d", x.CreatorID, got.Uses, want)
		}
	}
	// The record of a Worker that already enrolled is still returned.
	if res, err := s.EnrollWithInvitation(ctx, enrollParams(r, 1, "pi", 11)); err != nil || !res.Existing || res.Worker.ID != first.Worker.ID {
		t.Errorf("re-enroll after deactivation: %+v %v", res, err)
	}
}
