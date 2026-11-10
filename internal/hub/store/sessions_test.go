package store

import (
	"context"
	"errors"
	"testing"
)

func TestSessionLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("admin")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	row := SessionRow{ID: NewID(), TokenHash: "h1", UserID: p.User.ID, CredentialVersion: 1, CreatedAt: 10, ExpiresAt: 100}
	if err := s.CreateSession(ctx, row, 10); err != nil {
		t.Fatal(err)
	}
	got, u, err := s.SessionByTokenHash(ctx, "h1")
	if err != nil || got != row || u.ID != p.User.ID || u.Role != "admin" {
		t.Fatalf("SessionByTokenHash = %+v, %+v, %v", got, u, err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash err = %v", err)
	}
	if ok, err := s.DeleteSession(ctx, "h1"); err != nil || !ok {
		t.Fatalf("DeleteSession = %v, %v", ok, err)
	}
	if ok, err := s.DeleteSession(ctx, "h1"); err != nil || ok {
		t.Fatalf("second DeleteSession = %v, %v", ok, err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted session still found: %v", err)
	}
}

func TestCreateSessionPrunesExpiredAndEnforcesUniqueness(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("admin")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	old := SessionRow{ID: NewID(), TokenHash: "old", UserID: p.User.ID, CredentialVersion: 1, CreatedAt: 1, ExpiresAt: 50}
	if err := s.CreateSession(ctx, old, 1); err != nil {
		t.Fatal(err)
	}
	fresh := SessionRow{ID: NewID(), TokenHash: "fresh", UserID: p.User.ID, CredentialVersion: 1, CreatedAt: 60, ExpiresAt: 500}
	if err := s.CreateSession(ctx, fresh, 60); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session was not pruned: %v", err)
	}
	dup := fresh
	dup.ID = NewID()
	if err := s.CreateSession(ctx, dup, 60); err == nil {
		t.Error("a duplicate token hash was accepted")
	}
}

func TestSessionsAreDeletedWithTheirUser(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("admin")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, SessionRow{ID: NewID(), TokenHash: "h", UserID: p.User.ID, CredentialVersion: 1, CreatedAt: 1, ExpiresAt: 99}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, p.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, "h"); !errors.Is(err, ErrNotFound) {
		t.Errorf("session outlived its user: %v", err)
	}
}

func TestSessionForUnknownUserIsRefused(t *testing.T) {
	s := openTest(t)
	err := s.CreateSession(context.Background(), SessionRow{ID: NewID(), TokenHash: "h", UserID: "ghost", CredentialVersion: 1, CreatedAt: 1, ExpiresAt: 99}, 1)
	if err == nil {
		t.Fatal("a session for a missing user was accepted (foreign keys must be enforced)")
	}
}
