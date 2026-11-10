package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
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
	"0001_things.sql": "CREATE TABLE things (id TEXT PRIMARY KEY);",
}

var v2 = map[string]string{
	"0001_things.sql": "CREATE TABLE things (id TEXT PRIMARY KEY);",
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

func openStore(t *testing.T, dir string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// embeddedVersions returns the numbers of the embedded Worker migrations, so
// that a story that adds a migration does not have to edit these tests.
func embeddedVersions(t *testing.T) []int {
	t.Helper()
	entries, err := workerMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, e := range entries {
		n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			t.Fatalf("migration %q has no number: %v", e.Name(), err)
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func versions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, "backups"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestOpenCreatesDatabaseWithConnectionSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state") // does not exist yet
	s := openStore(t, dir)
	if s.Dir() != dir {
		t.Fatalf("Dir = %q", s.Dir())
	}
	if _, err := os.Stat(filepath.Join(dir, DBFile)); err != nil {
		t.Fatal(err)
	}
	if got := pragma(t, s.DB(), "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %q", got)
	}
	if got := pragma(t, s.DB(), "busy_timeout"); got != "5000" {
		t.Fatalf("busy_timeout = %q", got)
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
	// synchronous=FULL (2) on every connection: acknowledged commits are durable.
	for i, c := range []*sql.Conn{c1, c2} {
		var sync string
		if err := c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil || sync != "2" {
			t.Fatalf("connection %d synchronous = %q, %v; want 2 (FULL)", i+1, sync, err)
		}
	}
}

func TestOpenUsesImmediateTransactions(t *testing.T) {
	s := openStore(t, t.TempDir())
	// With _txlock=immediate a second write transaction cannot begin while the
	// first holds the write lock: it waits for busy_timeout and fails. A short
	// context keeps the test fast.
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	tx2, err := s.DB().BeginTx(ctx, nil)
	if err == nil {
		_ = tx2.Rollback()
		t.Fatal("second BEGIN succeeded while the first transaction holds the write lock; _txlock=immediate is not in effect")
	}
}

func TestOpenAppliesEmbeddedMigrations(t *testing.T) {
	s := openStore(t, t.TempDir())
	if got, want := versions(t, s.DB()), embeddedVersions(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %v, want the embedded set %v", got, want)
	}
	for _, table := range []string{"session_counter", "share_config", "share_selected", "op_results", "local_audit", "hub_enrollment"} {
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d, err=%v)", table, n, err)
		}
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
}

func TestFreshDatabaseWritesNoBackup(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	_ = s.Close()
	if got := backups(t, dir); len(got) != 0 {
		t.Fatalf("fresh database wrote backups %v", got)
	}
}

func TestReopenIsIdempotentAndWritesNoBackup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	if got, want := versions(t, s.DB()), embeddedVersions(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("versions after reopen = %v, want %v", got, want)
	}
	if got := backups(t, dir); len(got) != 0 {
		t.Fatalf("up-to-date reopen wrote backups %v", got)
	}
}

func TestPendingMigrationWritesBackupFirst(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO things (id) VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	fixed := time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC)
	s = openStore(t, dir, testMigrations(v2), WithClock(func() time.Time { return fixed }))
	got := backups(t, dir)
	if len(got) != 1 || !strings.HasPrefix(got[0], "worker-20261109T123000") || !strings.HasSuffix(got[0], ".db") {
		t.Fatalf("backups = %v, want one worker-<ts>.db", got)
	}
	if got := versions(t, s.DB()); len(got) != 2 {
		t.Fatalf("versions = %v", got)
	}

	// The backup holds the pre-migration state: one version, the data, no note column.
	bk, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "backups", got[0])+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer bk.Close()
	var n int
	if err := bk.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup schema_migrations rows = %d, %v; want 1", n, err)
	}
	if err := bk.QueryRow(`SELECT count(*) FROM things WHERE id='kept'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup lost data: %d, %v", n, err)
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v2))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A binary that knows only version 1 meets a database at version 2.
	_, err = Open(dir, testMigrations(v1))
	if !errors.Is(err, sqlmigrate.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
	if !strings.Contains(err.Error(), "schema_too_new") {
		t.Fatalf("error %q does not carry the code schema_too_new", err)
	}
	if got := backups(t, dir); len(got) != 0 {
		t.Fatalf("a refused open wrote backups %v", got)
	}
}

func TestNewerSchemaIsRefusedWithEmbeddedMigrations(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (99, 'future', 0)`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(dir); !errors.Is(err, sqlmigrate.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
}

func TestBrokenMigrationLeavesSchemaUntouched(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	bad := map[string]string{
		"0001_things.sql": v1["0001_things.sql"],
		"0002_bad.sql":    "ALTER TABLE things ADD COLUMN ok TEXT; THIS IS NOT SQL;",
	}
	if _, err := Open(dir, testMigrations(bad)); err == nil {
		t.Fatal("broken migration was applied")
	}
	s = openStore(t, dir, testMigrations(v1))
	if got := versions(t, s.DB()); len(got) != 1 {
		t.Fatalf("versions = %v, want [1]", got)
	}
	if _, err := s.DB().Exec(`SELECT ok FROM things`); err == nil {
		t.Fatal("half of the broken migration was kept")
	}
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are checked on Unix; see db_windows_test.go")
	}
	// A pre-existing state dir with a wide mode and a permissive umask.
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dir)
	if _, err := s.NextSessionNumber(); err != nil { // forces a WAL write
		t.Fatal(err)
	}
	for _, f := range []string{DBFile, DBFile + "-wal", DBFile + "-shm"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", f, fi.Mode().Perm())
		}
	}
}

func TestOpenCreatesStateDirOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	dir := filepath.Join(t.TempDir(), "a", "state")
	openStore(t, dir)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state dir mode = %04o, want no group/other access", fi.Mode().Perm())
	}
}

func TestOpenTightensExistingDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	p := filepath.Join(dir, DBFile)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	openStore(t, dir)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o, want 0600", fi.Mode().Perm())
	}
}

func TestOpenReportsUnwritableStateDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(filepath.Join(file, "state"))
	if err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("err = %v, want a state directory error", err)
	}
}
