package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

func testMigrations(files map[string]string) Option {
	m := fstest.MapFS{}
	for name, body := range files {
		m["migrations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return WithMigrations(m, "migrations")
}

var v1 = map[string]string{
	"0001_things.sql": "CREATE TABLE things (id TEXT PRIMARY KEY, body BLOB);",
}

var v2 = map[string]string{
	"0001_things.sql": "CREATE TABLE things (id TEXT PRIMARY KEY, body BLOB);",
	"0002_more.sql":   "ALTER TABLE things ADD COLUMN note TEXT;",
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenCreatesDatabaseWithConnectionSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data") // does not exist yet
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Dir() != dir {
		t.Fatalf("Dir = %q", s.Dir())
	}
	for _, f := range []string{DBFile, DataKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if got := pragma(t, s.DB(), "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %q", got)
	}
	if got := pragma(t, s.DB(), "busy_timeout"); got != "5000" {
		t.Fatalf("busy_timeout = %q", got)
	}
	if got := pragma(t, s.DB(), "foreign_keys"); got != "1" {
		t.Fatalf("foreign_keys = %q", got)
	}
	// The pragmas hold on every pooled connection, not only the first one.
	ctx := context.Background()
	c1, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var bt string
	if err := c2.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil || bt != "5000" {
		t.Fatalf("second connection busy_timeout = %q, %v", bt, err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Fatalf("data dir mode = %v", st.Mode().Perm())
		}
	}
}

func TestOpenUsesImmediateTransactions(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx1, err := s.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback() }()
	// With _txlock=immediate the second BeginTx must wait for the write lock
	// held by tx1; a deferred transaction would begin at once.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	tx2, err := s.DB().BeginTx(ctx, nil)
	if err == nil {
		_ = tx2.Rollback()
		t.Fatal("second transaction began while the first holds the write lock: not immediate")
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("second BeginTx failed after %v: %v", time.Since(start), err)
	}
}

func TestOpenAppliesMigrationsAndKeepsThemOnReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO things (id, body) VALUES ('a', x'01')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := os.Stat(filepath.Join(dir, "backups")); err == nil {
		t.Fatal("first start must not write a backup")
	}

	s, err = Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM things`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v", n, err)
	}
}

func TestOpenBacksUpBeforeMigrating(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO things (id, body) VALUES ('a', x'01')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	fixed := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	s, err = Open(dir, testMigrations(v2), WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backup := filepath.Join(dir, "backups", "minion-20261109T080000.000Z.db")
	if _, err := os.Stat(backup); err != nil {
		ents, _ := os.ReadDir(filepath.Join(dir, "backups"))
		t.Fatalf("backup %s: %v (have %v)", backup, err, ents)
	}
	// The new column exists in the live database only.
	if _, err := s.DB().Exec(`UPDATE things SET note = 'x'`); err != nil {
		t.Fatal(err)
	}
	b, err := sql.Open("sqlite", "file:"+backup+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Exec(`UPDATE things SET note = 'x'`); err == nil {
		t.Fatal("backup contains the post-migration schema")
	}
	var n int
	if err := b.QueryRow(`SELECT count(*) FROM things`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup rows = %d, %v", n, err)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v2))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	_, err = Open(dir, testMigrations(v1))
	if !errors.Is(err, sqlmigrate.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want schema_too_new", err)
	}
	if !strings.Contains(err.Error(), "schema_too_new") {
		t.Fatalf("error text lacks the code: %q", err)
	}
}

func TestOpenDefaultMigrationsEmbedded(t *testing.T) {
	// The embedded set is valid whatever stories have added to it.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDataKeyIsStable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Box().Seal("t", "c", "r", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Box().Open("t", "c", "r", sealed)
	if err != nil || string(got) != "secret" {
		t.Fatalf("after reopen: %q, %v", got, err)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("empty dir must fail")
	}
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(file, "sub")); err == nil {
		t.Fatal("a data dir below a file must fail")
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		ro := t.TempDir()
		if err := os.Chmod(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(ro, 0o700) }()
		_, err := Open(ro)
		if err == nil || !strings.Contains(err.Error(), "not writable") || !strings.Contains(err.Error(), "65532") {
			t.Fatalf("read-only dir err = %v", err)
		}
	}
}

func TestOpenDataDirWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my data #1?x")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(filepath.Join(dir, DBFile)); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBadMigrationLeavesNoPartialSchema(t *testing.T) {
	dir := t.TempDir()
	_, err := Open(dir, testMigrations(map[string]string{
		"0001_ok.sql":  "CREATE TABLE ok (id INTEGER);",
		"0002_bad.sql": "NOT SQL;",
	}))
	if err == nil || !strings.Contains(err.Error(), "0002_bad") {
		t.Fatalf("err = %v", err)
	}
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='ok'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("table ok exists after failed run: %d, %v", n, err)
	}
}

func TestOpenDatabaseFilesAreOwnerOnlyInExistingWideDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // the Docker bind-mount case
		t.Fatal(err)
	}
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.DB().Exec(`INSERT INTO things (id) VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DBFile, DBFile + "-wal"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}

func TestOpenTightensExistingDatabaseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := os.Chmod(filepath.Join(dir, DBFile), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	fi, err := os.Stat(filepath.Join(dir, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
}

func TestOpenRefusesMissingKeyForExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	keyPath := filepath.Join(dir, DataKeyFile)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	_, err = Open(dir, testMigrations(v1))
	if err == nil || !strings.Contains(err.Error(), "data key") || !strings.Contains(err.Error(), DataKeyFile) {
		t.Fatalf("err = %v, want an actionable missing-key error", err)
	}
	if _, statErr := os.Stat(keyPath); statErr == nil {
		t.Fatal("a replacement key must not be created")
	}
}

func TestOpenTightensRestoredWALAndSHM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO things (id) VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	// Capture the sidecars while the database is open (nonempty WAL and SHM),
	// then restore them next to the main file with wide modes, as a restore
	// from a file-level backup would.
	files := map[string][]byte{}
	for _, name := range []string{DBFile, DBFile + "-wal", DBFile + "-shm"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			t.Fatalf("%s is empty", name)
		}
		files[name] = b
	}
	_ = s.Close()
	restored := filepath.Join(t.TempDir(), "restored")
	if err := os.Mkdir(restored, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		p := filepath.Join(restored, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(filepath.Join(dir, DataKeyFile), filepath.Join(restored, DataKeyFile)); err != nil {
		t.Fatal(err)
	}
	s, err = Open(restored, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM things`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("restored rows = %d, %v", n, err)
	}
	for name := range files {
		fi, err := os.Stat(filepath.Join(restored, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}
