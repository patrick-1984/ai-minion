// Package hubstore gives tests a Hub data directory whose database is already
// migrated. The migration runs once per test binary (through the real
// store.Open) and its files are copied into each new directory, because under
// the race detector a migration costs several hundred milliseconds and many
// tests only need a ready Store. A test of the first migration, of key
// handling or of an upgrade must not use it. Test support only.
package hubstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/testutil/dircache"
)

// Seed fills the empty directory dir with a migrated Hub database and its data
// key.
func Seed(t testing.TB, dir string) {
	t.Helper()
	dircache.Seed(t, "hubstore-fresh", dir, func(d string) error {
		s, err := store.Open(d)
		if err != nil {
			return err
		}
		return s.Close()
	})
}

// Dir returns a new test directory that Seed has filled.
func Dir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	Seed(t, dir)
	return dir
}

// Open is store.Open on a directory that is seeded first when it has no
// database yet.
func Open(t testing.TB, dir string) (*store.Store, error) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, store.DBFile)); os.IsNotExist(err) {
		Seed(t, dir)
	}
	return store.Open(dir)
}
