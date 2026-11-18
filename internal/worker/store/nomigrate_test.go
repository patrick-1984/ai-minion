package store

import (
	"context"
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
