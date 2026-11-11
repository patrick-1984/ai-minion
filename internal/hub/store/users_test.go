package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedParams(name string) SeedAdminParams {
	return SeedAdminParams{
		User: UserRow{
			ID: NewID(), Username: name, PasswordHash: "hash-" + name,
			MustChangePassword: true, CredentialVersion: 1, CreatedAt: 1000, UpdatedAt: 1000,
		},
		Marker: "admin_seed",
		Audit:  &AuditRow{ID: NewID(), TS: 1000, Actor: "system", Action: "user.seed", Target: "t", Outcome: "success"},
	}
}

func TestSeedAdminCreatesOnceInOneTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if has, err := s.HasUsers(ctx); err != nil || has {
		t.Fatalf("HasUsers on an empty table = %v, %v", has, err)
	}
	p := seedParams("Admin")
	created, err := s.SeedAdmin(ctx, p)
	if err != nil || !created {
		t.Fatalf("SeedAdmin = %v, %v", created, err)
	}
	u, err := s.UserByID(ctx, p.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != "admin" || !u.Active || !u.MustChangePassword || u.PasswordHash != "hash-Admin" || u.CredentialVersion != 1 {
		t.Errorf("seeded user = %+v", u)
	}
	var marker string
	if err := s.db.QueryRow(`SELECT value FROM auth_meta WHERE key = 'admin_seed'`).Scan(&marker); err != nil || marker != p.User.ID {
		t.Errorf("marker = %q, %v", marker, err)
	}
	rows, err := s.ListAudit(ctx, AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Action != "user.seed" {
		t.Errorf("audit rows = %+v, %v", rows, err)
	}

	// A second seed does nothing, and never touches the password.
	p2 := seedParams("other")
	created, err = s.SeedAdmin(ctx, p2)
	if err != nil || created {
		t.Fatalf("second SeedAdmin = %v, %v", created, err)
	}
	if _, err := s.UserByID(ctx, p2.User.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second user exists: %v", err)
	}
	again, _ := s.UserByID(ctx, p.User.ID)
	if again.PasswordHash != u.PasswordHash {
		t.Error("the password hash changed")
	}
	if rows, _ := s.ListAudit(ctx, AuditQuery{Limit: 10}); len(rows) != 1 {
		t.Errorf("a no-op seed wrote an audit row: %d rows", len(rows))
	}
}

func TestSeedAdminDoesNothingWhenTheMarkerExists(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO auth_meta (key, value, created_at) VALUES ('admin_seed', 'earlier-user', 500)`); err != nil {
		t.Fatal(err)
	}
	created, err := s.SeedAdmin(ctx, seedParams("admin"))
	if err != nil || created {
		t.Fatalf("SeedAdmin with the marker present = %v, %v; want false, nil", created, err)
	}
	for _, table := range []string{"users", "user_roles", "audit_events"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s has %d rows (%v), want none", table, n, err)
		}
	}
	var value string
	var ts int64
	if err := s.db.QueryRow(`SELECT value, created_at FROM auth_meta WHERE key = 'admin_seed'`).Scan(&value, &ts); err != nil || value != "earlier-user" || ts != 500 {
		t.Errorf("the marker was overwritten: %q %d %v", value, ts, err)
	}
}

func TestSeedAdminRollsBackEverythingOnFailure(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("admin")
	// An audit row with an id that already exists makes the last statement of
	// the transaction fail with a primary key violation.
	if err := s.InsertAudit(ctx, nil, *p.Audit); err != nil {
		t.Fatal(err)
	}
	if created, err := s.SeedAdmin(ctx, p); err == nil || created {
		t.Fatalf("SeedAdmin = %v, %v; want an error", created, err)
	}
	if has, _ := s.HasUsers(ctx); has {
		t.Error("the user row survived a failed seed")
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM auth_meta`).Scan(&n)
	if n != 0 {
		t.Error("the marker survived a failed seed")
	}
	_ = s.db.QueryRow(`SELECT count(*) FROM user_roles`).Scan(&n)
	if n != 0 {
		t.Error("the role grant survived a failed seed")
	}
}

func TestSeedAdminConcurrentStartsCreateOneUser(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	made := 0
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := seedParams("admin" + string(rune('a'+i)))
			p.Audit = nil
			ok, err := s.SeedAdmin(ctx, p)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				made++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if made != 1 {
		t.Fatalf("%d seeds succeeded, want 1", made)
	}
}

func TestSeedAdminValidatesInput(t *testing.T) {
	s := openTest(t)
	p := seedParams("admin")
	p.Marker = ""
	if _, err := s.SeedAdmin(context.Background(), p); err == nil {
		t.Fatal("empty marker accepted")
	}
}

func TestUserByIdentifier(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("Alice")
	p.User.Email = "Alice@Example.com"
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, ident := range []string{"Alice", "alice", "ALICE", "alice@example.com", "ALICE@EXAMPLE.COM"} {
		u, err := s.UserByIdentifier(ctx, ident)
		if err != nil || u.ID != p.User.ID {
			t.Errorf("UserByIdentifier(%q) = %+v, %v", ident, u, err)
		}
	}
	for _, ident := range []string{"", "bob", "alice@example.org", "%"} {
		if _, err := s.UserByIdentifier(ctx, ident); !errors.Is(err, ErrNotFound) {
			t.Errorf("UserByIdentifier(%q) err = %v, want ErrNotFound", ident, err)
		}
	}
}

func TestUsernameAndEmailAreUnique(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("alice")
	p.User.Email = "a@example.com"
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"username (case-insensitive)": `INSERT INTO users (id, username, created_at, updated_at) VALUES ('x1', 'ALICE', 1, 1)`,
		"email (case-insensitive)":    `INSERT INTO users (id, username, email, created_at, updated_at) VALUES ('x2', 'bob', 'A@EXAMPLE.COM', 1, 1)`,
	} {
		if _, err := s.db.Exec(q); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			t.Errorf("duplicate %s accepted: %v", name, err)
		}
	}
	// Several accounts without an e-mail address are fine.
	for _, id := range []string{"y1", "y2"} {
		if _, err := s.db.Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1)`, id, id); err != nil {
			t.Errorf("account without e-mail refused: %v", err)
		}
	}
}

func TestRoleIsAdminOnlyWithAnAdminGrant(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES ('u1', 'plain', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByID(ctx, "u1")
	if err != nil || u.Role != "" {
		t.Fatalf("an account without grants has role %q, %v; want none (F-07)", u.Role, err)
	}
	if _, err := s.db.Exec(`INSERT INTO user_roles VALUES ('u1', 'user', 'local')`); err != nil {
		t.Fatal(err)
	}
	if u, err = s.UserByID(ctx, "u1"); err != nil || u.Role != "user" {
		t.Fatalf("role with a user grant = %q, %v", u.Role, err)
	}
	if _, err := s.db.Exec(`INSERT INTO user_roles VALUES ('u1', 'admin', 'idp')`); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.UserByID(ctx, "u1"); u.Role != "admin" {
		t.Fatalf("role with an idp admin grant = %q", u.Role)
	}
}
