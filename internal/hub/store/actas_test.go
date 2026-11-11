package store

import (
	"context"
	"errors"
	"testing"
)

// actAsFixture seeds an admin with a session and a plain user.
func actAsFixture(t *testing.T) (s *Store, sessionID, adminID, userID string) {
	t.Helper()
	s = openTest(t)
	ctx := context.Background()
	p := seedParams("admin")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	adminID, userID, sessionID = p.User.ID, NewID(), NewID()
	if _, err := s.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, 'ann', 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, userID); err != nil {
		t.Fatal(err)
	}
	row := SessionRow{ID: sessionID, TokenHash: "h", UserID: adminID, CredentialVersion: 1, CreatedAt: 1, ExpiresAt: 1 << 40}
	if err := s.CreateSession(ctx, row, 1); err != nil {
		t.Fatal(err)
	}
	return s, sessionID, adminID, userID
}

func TestActAsLifecycle(t *testing.T) {
	s, sid, admin, user := actAsFixture(t)
	ctx := context.Background()
	if _, err := s.ActAsBySession(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ActAsBySession before start = %v", err)
	}
	row := ActAsRow{SessionID: sid, ActorID: admin, TargetID: user, StartedAt: 10, ExpiresAt: 20}
	if err := s.StartActAs(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActAsBySession(ctx, sid)
	if err != nil || got.ActAsRow != row || got.TargetUsername != "ann" || !got.TargetActive {
		t.Fatalf("ActAsBySession = %+v, %v", got, err)
	}
	if err := s.StartActAs(ctx, row); !errors.Is(err, ErrActAsActive) {
		t.Fatalf("second StartActAs = %v, want ErrActAsActive", err)
	}
	if ended, err := s.EndActAs(ctx, sid); err != nil || !ended {
		t.Fatalf("EndActAs = %v, %v", ended, err)
	}
	if ended, err := s.EndActAs(ctx, sid); err != nil || ended {
		t.Fatalf("second EndActAs = %v, %v", ended, err)
	}
	if _, err := s.ActAsBySession(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ended act-as still found: %v", err)
	}
}

func TestStartActAsRejectsSelfAndBadWindow(t *testing.T) {
	s, sid, admin, user := actAsFixture(t)
	ctx := context.Background()
	if err := s.StartActAs(ctx, ActAsRow{SessionID: sid, ActorID: admin, TargetID: admin, StartedAt: 1, ExpiresAt: 2}); err == nil {
		t.Error("acting as oneself was accepted")
	}
	if err := s.StartActAs(ctx, ActAsRow{SessionID: sid, ActorID: admin, TargetID: user, StartedAt: 5, ExpiresAt: 5}); err == nil {
		t.Error("an empty window was accepted")
	}
	if err := s.StartActAs(ctx, ActAsRow{SessionID: NewID(), ActorID: admin, TargetID: user, StartedAt: 1, ExpiresAt: 2}); err == nil {
		t.Error("an unknown session was accepted")
	}
}

func TestEndActAsIfWritesTheAuditRowOnlyWhenTheObservedPeriodWasDeleted(t *testing.T) {
	s, sid, admin, user := actAsFixture(t)
	ctx := context.Background()
	old := ActAsRow{SessionID: sid, ActorID: admin, TargetID: user, StartedAt: 1, ExpiresAt: 2}
	if err := s.StartActAs(ctx, old); err != nil {
		t.Fatal(err)
	}
	audit := func() AuditRow {
		return AuditRow{ID: NewID(), TS: 3, Actor: admin, ActingAs: user, Action: "act_as.expire", Target: user, Outcome: "success"}
	}
	for i, want := range []bool{true, false} {
		if ended, err := s.EndActAsIf(ctx, old, audit()); err != nil || ended != want {
			t.Fatalf("EndActAsIf #%d = %v, %v", i, ended, err)
		}
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'act_as.expire' AND acting_as = ?`, user).Scan(&n); err != nil || n != 1 {
		t.Fatalf("expire audit rows = %d, %v; want exactly 1", n, err)
	}
}

// A request that read an old period must not end, or audit the end of, a
// period that was started after it read.
func TestEndActAsIfDoesNotHitANewerPeriod(t *testing.T) {
	s, sid, admin, user := actAsFixture(t)
	ctx := context.Background()
	old := ActAsRow{SessionID: sid, ActorID: admin, TargetID: user, StartedAt: 1, ExpiresAt: 2}
	if err := s.StartActAs(ctx, old); err != nil {
		t.Fatal(err)
	}
	// Another request ends the old period and starts a new one.
	if ended, err := s.EndActAs(ctx, sid); err != nil || !ended {
		t.Fatalf("EndActAs = %v, %v", ended, err)
	}
	fresh := ActAsRow{SessionID: sid, ActorID: admin, TargetID: user, StartedAt: 5, ExpiresAt: 50}
	if err := s.StartActAs(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	audit := AuditRow{ID: NewID(), TS: 6, Actor: admin, ActingAs: user, Action: "act_as.expire", Target: user, Outcome: "success"}
	if ended, err := s.EndActAsIf(ctx, old, audit); err != nil || ended {
		t.Fatalf("EndActAsIf of the old period = %v, %v; want false, nil", ended, err)
	}
	got, err := s.ActAsBySession(ctx, sid)
	if err != nil || got.ActAsRow != fresh {
		t.Fatalf("the new period was touched: %+v, %v", got, err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'act_as.expire'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("audit rows for a period that was not ended = %d, %v; want 0", n, err)
	}
}

func TestActAsBySessionReportsADeactivatedTarget(t *testing.T) {
	s, sid, admin, user := actAsFixture(t)
	ctx := context.Background()
	if err := s.StartActAs(ctx, ActAsRow{SessionID: sid, ActorID: admin, TargetID: user, StartedAt: 1, ExpiresAt: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, user); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActAsBySession(ctx, sid)
	if err != nil || got.TargetActive {
		t.Fatalf("ActAsBySession = %+v, %v; want TargetActive false", got, err)
	}
}

func TestActAsIsDeletedWithItsSessionAndUsers(t *testing.T) {
	s, sid, admin, user := actAsFixture(t)
	ctx := context.Background()
	if err := s.StartActAs(ctx, ActAsRow{SessionID: sid, ActorID: admin, TargetID: user, StartedAt: 1, ExpiresAt: 2}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteSession(ctx, "h"); err != nil || !ok {
		t.Fatalf("DeleteSession = %v, %v", ok, err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM act_as_sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("act-as rows after session delete = %d, %v", n, err)
	}
}
