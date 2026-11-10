//go:build unix

package sqlmigrate_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

// TestBackupIsOwnerOnlyWhileCopying proves that the backup is never readable
// by other users, not even while VACUUM INTO is still writing it: the backup
// directory exists with mode 0755 and the umask is 022.
func TestBackupIsOwnerOnlyWhileCopying(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	if _, err := sqlmigrate.Apply(context.Background(), db, fixture("v1")); err != nil {
		t.Fatal(err)
	}
	// Enough data that the copy takes a while.
	if _, err := db.Exec(`CREATE TABLE bulk (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := db.Exec(`INSERT INTO bulk VALUES (zeroblob(4000000))`); err != nil {
			t.Fatal(err)
		}
	}
	backups := filepath.Join(dir, "backups")
	if err := os.Mkdir(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	fixed := time.Date(2026, 11, 9, 12, 30, 45, 123_000_000, time.UTC)
	want := filepath.Join(backups, "minion-20261109T123045.123Z.db")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var seen, wide int
	var widest os.FileMode
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if fi, err := os.Stat(want); err == nil {
				seen++
				if fi.Mode().Perm()&0o077 != 0 {
					wide++
					widest = fi.Mode().Perm()
				}
			}
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
			}
		}
	}()
	_, err := sqlmigrate.Apply(context.Background(), db, sqlmigrate.Options{
		FS: os.DirFS("testdata"), Dir: "good", BackupDir: backups, BackupPrefix: "minion",
		Now: func() time.Time { return fixed },
	})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("the backup file was never observed while copying")
	}
	if wide != 0 {
		t.Fatalf("backup was group/world accessible (%04o) in %d of %d observations", widest, wide, seen)
	}
}
