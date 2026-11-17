package store

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestUserPrefsDefaultAndSet(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seedParams("pat")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserPrefs(ctx, p.User.ID)
	if err != nil || got.AllowHiddenSessions || got.UpdatedAt != 0 {
		t.Fatalf("default prefs = %+v, %v; want hidden sessions off", got, err)
	}
	if err := s.SetAllowHiddenSessions(ctx, p.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserPrefs(ctx, p.User.ID); err != nil || !got.AllowHiddenSessions || got.UpdatedAt == 0 {
		t.Fatalf("after allow = %+v, %v", got, err)
	}
	if err := s.SetAllowHiddenSessions(ctx, p.User.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserPrefs(ctx, p.User.ID); got.AllowHiddenSessions {
		t.Fatalf("after disallow = %+v", got)
	}
	if err := s.SetAllowHiddenSessions(ctx, NewID(), true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown user = %v, want ErrNotFound", err)
	}
	// The row goes with its user.
	if err := s.SetAllowHiddenSessions(ctx, p.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, p.User.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM user_prefs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user_prefs rows after the user was deleted = %d, %v", n, err)
	}
}

// A database of the previous schema (up to 0025) gains the table on upgrade,
// and its existing users get the defaults.
func TestUserPrefsMigrationUpgradesAnExistingDatabase(t *testing.T) {
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(hubMigrations, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0026" {
			continue
		}
		b, err := fs.ReadFile(hubMigrations, migrationsDir+"/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		old[migrationsDir+"/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	dir := t.TempDir()
	s, err := Open(dir, WithMigrations(old, migrationsDir))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := seedParams("pat")
	if _, err := s.SeedAdmin(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`SELECT 1 FROM user_prefs`); err == nil {
		t.Fatal("the old schema already has user_prefs")
	}
	_ = s.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.UserPrefs(ctx, p.User.ID); err != nil || got.AllowHiddenSessions {
		t.Fatalf("prefs of an existing user after the upgrade = %+v, %v", got, err)
	}
	if err := s.SetAllowHiddenSessions(ctx, p.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE user_prefs SET allow_hidden_sessions = 2`); err == nil {
		t.Error("the CHECK constraint accepted allow_hidden_sessions = 2")
	}
}
