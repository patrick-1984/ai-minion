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

func TestSessionAuthIsStoredAndOnlyTheServerSetsIt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("admin")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	plain := SessionRow{ID: NewID(), TokenHash: "plain", UserID: p.User.ID, CredentialVersion: 1, CreatedAt: 10, ExpiresAt: 100}
	if err := s.CreateSession(ctx, plain, 10); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.SessionByTokenHash(ctx, "plain")
	if err != nil || got.AuthAt != 0 || got.AuthMethod != "" {
		t.Fatalf("a session without a credential check = %+v, %v (migration default must be 0)", got, err)
	}
	checked := SessionRow{ID: NewID(), TokenHash: "checked", UserID: p.User.ID, CredentialVersion: 1, CreatedAt: 10, ExpiresAt: 100, AuthAt: 55, AuthMethod: "local"}
	if err := s.CreateSession(ctx, checked, 10); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.SessionByTokenHash(ctx, "checked"); got.AuthAt != 55 || got.AuthMethod != "local" {
		t.Errorf("stored auth = %d %q", got.AuthAt, got.AuthMethod)
	}
	if ok, err := s.SetSessionAuth(ctx, plain.ID, 77, "oidc:corp"); err != nil || !ok {
		t.Fatalf("SetSessionAuth = %v, %v", ok, err)
	}
	if got, _, _ := s.SessionByTokenHash(ctx, "plain"); got.AuthAt != 77 || got.AuthMethod != "oidc:corp" {
		t.Errorf("raised auth = %d %q", got.AuthAt, got.AuthMethod)
	}
	// The other session is untouched; a missing session is reported.
	if got, _, _ := s.SessionByTokenHash(ctx, "checked"); got.AuthAt != 55 {
		t.Errorf("another session changed: %d", got.AuthAt)
	}
	if ok, err := s.SetSessionAuth(ctx, "no-such-session", 1, "local"); err != nil || ok {
		t.Errorf("SetSessionAuth on a missing session = %v, %v", ok, err)
	}
}

func TestExternalIdentityOfNamesTheSubjectAtAProvider(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, _, err := s.ExternalIdentityOf(ctx, "nobody", "corp"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	p := seedParams("admin")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO external_identities (id, user_id, issuer, subject, provider_id, created_at, last_login_at) VALUES (?, ?, 'https://idp', 'sub-1', 'corp', 1, 1)`, NewID(), p.User.ID); err != nil {
		t.Fatal(err)
	}
	iss, sub, err := s.ExternalIdentityOf(ctx, p.User.ID, "corp")
	if err != nil || iss != "https://idp" || sub != "sub-1" {
		t.Errorf("identity = %q %q %v", iss, sub, err)
	}
	if _, _, err := s.ExternalIdentityOf(ctx, p.User.ID, "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("other provider: %v", err)
	}
}
