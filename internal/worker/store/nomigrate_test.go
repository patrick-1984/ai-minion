package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

func appliedOf(t *testing.T, dir string) []int {
	t.Helper()
	ro, err := OpenReadOnlyContext(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	v, _, err := sqlmigrate.Applied(context.Background(), ro.db)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func backupsOf(dir string) int {
	e, _ := os.ReadDir(filepath.Join(dir, backupDirName))
	return len(e)
}

// NoMigrate refuses a database that is older or newer than the binary,
// before anything is written: the applied versions and the backups stay.
func TestNoMigrateRefusesOlderAndNewer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		db, open map[string]string
	}{
		{"older database", v1, v2},
		{"newer database", v2, v1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := Open(dir, testMigrations(tc.db))
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			before, backups := appliedOf(t, dir), backupsOf(dir)
			_, err = Open(dir, testMigrations(tc.open), NoMigrate())
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("open = %v, want ErrSchemaMismatch", err)
			}
			if after := appliedOf(t, dir); !slices.Equal(after, before) || backupsOf(dir) != backups {
				t.Fatalf("versions %v -> %v, backups %d -> %d", before, after, backups, backupsOf(dir))
			}
		})
	}
	// The same schema opens; a new state directory gets this schema.
	dir := t.TempDir()
	st, err := Open(dir, testMigrations(v2))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = Open(dir, testMigrations(v2), NoMigrate())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	fresh := t.TempDir()
	st, err = Open(fresh, testMigrations(v2), NoMigrate())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if v := appliedOf(t, fresh); !slices.Equal(v, []int{1, 2}) {
		t.Fatalf("new database at %v", v)
	}
	// The real embedded schema opens with NoMigrate.
	real := t.TempDir()
	st, err = Open(real)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = Open(real, NoMigrate())
	if err != nil {
		t.Fatalf("the embedded schema: %v", err)
	}
	_ = st.Close()
}

// NoMigrate never migrates an existing file, whatever it holds: an empty
// file (an older Worker that created it and has not migrated yet), a database
// with tables but no migration ledger and a partial schema are refused with
// ErrSchemaMismatch, and the file is left as it was (no ledger is created).
func TestNoMigrateRefusesEmptyAndUnledgeredDatabases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"empty file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"tables without a ledger", func(t *testing.T, path string) {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec("CREATE TABLE things (id TEXT PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, DBFile)
			tc.setup(t, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			st, err := Open(dir, testMigrations(v2), NoMigrate())
			if err == nil {
				_ = st.Close()
			}
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("open = %v, want ErrSchemaMismatch", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("the refused database file was changed")
			}
			if backupsOf(dir) != 0 {
				t.Fatal("a refused open wrote a backup")
			}
		})
	}
}

// An older Worker that initializes the database while a NoMigrate open is
// under way (between its check and its own write) keeps its database: the
// open is refused with ErrSchemaMismatch and the database stays at the older
// Worker's schema, never migrated to this binary's.
func TestNoMigrateDoesNotUpgradeConcurrentlyInitializedDatabase(t *testing.T) {
	dir := t.TempDir()
	testHookNoMigrateFresh = func() {
		st, err := Open(dir, testMigrations(v1)) // the older Worker starts
		if err != nil {
			t.Error(err)
			return
		}
		_ = st.Close()
	}
	t.Cleanup(func() { testHookNoMigrateFresh = nil })
	st, err := Open(dir, testMigrations(v2), NoMigrate())
	if err == nil {
		_ = st.Close()
	}
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("open = %v, want ErrSchemaMismatch", err)
	}
	if v := appliedOf(t, dir); !slices.Equal(v, []int{1}) {
		t.Fatalf("the older Worker's database is at %v, want [1]", v)
	}
	// The older Worker still opens its database.
	st, err = Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatalf("the older Worker cannot reopen its database: %v", err)
	}
	_ = st.Close()
}

// A schema mismatch is reported before the state directory's permissions are
// touched (on Windows protectStateDir replaces the directory DACL).
func TestNoMigrateMismatchLeavesStateDirPermissions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	called := false
	prev := protectDir
	protectDir = func(string) error { called = true; return nil }
	t.Cleanup(func() { protectDir = prev })
	if _, err := Open(dir, testMigrations(v2), NoMigrate()); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("open = %v, want ErrSchemaMismatch", err)
	}
	if called {
		t.Fatal("the state directory was protected (its ACL replaced) before the schema check refused the open")
	}
}
