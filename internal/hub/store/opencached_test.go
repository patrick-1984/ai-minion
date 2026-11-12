package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/testutil/dircache"
)

// openCached is Open for a test that is not about the first migration of a
// fresh data directory. When dir has no database yet it is first filled with
// the files of a directory that the real Open created and migrated once for
// this test binary (under the race detector a migration costs several hundred
// milliseconds); then the real Open runs on it, so the open path, the key
// handling and the migration check are still exercised. Tests of the fresh
// migration, of key handling and of upgrades call Open themselves.
func openCached(t testing.TB, dir string) (*Store, error) {
	t.Helper()
	if abs, err := filepath.Abs(dir); err == nil {
		if _, err := os.Stat(filepath.Join(abs, DBFile)); os.IsNotExist(err) {
			dircache.Seed(t, "store-fresh", abs, func(d string) error {
				s, err := Open(d)
				if err != nil {
					return err
				}
				return s.Close()
			})
		}
	}
	return Open(dir)
}
