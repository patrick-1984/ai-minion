package sqlmigrate_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	_ "modernc.org/sqlite"

	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

func openDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "t.db")+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fixture(name string) sqlmigrate.Options {
	return sqlmigrate.Options{FS: os.DirFS("testdata"), Dir: name}
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

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestApplyFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	opts := fixture("good")
	opts.BackupDir = filepath.Join(dir, "backups")
	res, err := sqlmigrate.Apply(context.Background(), db, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || res.Applied[0] != 1 || res.Applied[1] != 2 || res.Current != 2 {
		t.Fatalf("result = %+v", res)
	}
	if got := versions(t, db); len(got) != 2 {
		t.Fatalf("versions = %v", got)
	}
	// Both migrations took effect: the seed row, the new column and the index.
	var note sql.NullString
	if err := db.QueryRow(`SELECT note FROM items WHERE id = 1`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	// A fresh database holds nothing to save, so no backup is written.
	if res.Backup != "" {
		t.Fatalf("fresh database wrote a backup: %s", res.Backup)
	}
	if _, err := os.Stat(opts.BackupDir); err == nil {
		t.Fatal("backup directory created for a fresh database")
	}
}

func TestApplyRerunIsNoop(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	opts := fixture("good")
	opts.BackupDir = filepath.Join(dir, "backups")
	if _, err := sqlmigrate.Apply(context.Background(), db, opts); err != nil {
		t.Fatal(err)
	}
	res, err := sqlmigrate.Apply(context.Background(), db, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || res.Current != 2 || res.Backup != "" {
		t.Fatalf("re-run result = %+v", res)
	}
	if _, err := os.Stat(opts.BackupDir); err == nil {
		t.Fatal("no-op run created a backup directory")
	}
}

func TestApplyWritesBackupBeforeUpgrade(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	backups := filepath.Join(dir, "backups")
	if _, err := sqlmigrate.Apply(context.Background(), db, fixture("v1")); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 11, 9, 12, 30, 45, 123_000_000, time.UTC)
	res, err := sqlmigrate.Apply(context.Background(), db, sqlmigrate.Options{
		FS: os.DirFS("testdata"), Dir: "good", BackupDir: backups, BackupPrefix: "minion",
		Now: func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(backups, "minion-20261109T123045.123Z.db")
	if res.Backup != want {
		t.Fatalf("backup = %q, want %q", res.Backup, want)
	}
	st, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 && filepath.Separator == '/' {
		t.Fatalf("backup mode = %v, want owner only", st.Mode().Perm())
	}
	// The backup is the pre-migration state: version 1 only, data intact.
	b, err := sql.Open("sqlite", "file:"+want+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n int
	if err := b.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup versions = %d, %v", n, err)
	}
	if err := b.QueryRow(`SELECT count(*) FROM items`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup items = %d, %v", n, err)
	}
	if _, err := b.Query(`SELECT note FROM items`); err == nil {
		t.Fatal("backup already has the new column")
	}
	if got := versions(t, db); len(got) != 2 {
		t.Fatalf("live versions = %v", got)
	}
}

func TestApplyRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	backups := filepath.Join(dir, "backups")
	if _, err := sqlmigrate.Apply(context.Background(), db, fixture("good")); err != nil {
		t.Fatal(err)
	}
	// An older binary knows only version 1.
	opts := fixture("v1")
	opts.BackupDir = backups
	_, err := sqlmigrate.Apply(context.Background(), db, opts)
	if !errors.Is(err, sqlmigrate.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
	if !strings.Contains(err.Error(), "schema_too_new") || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("error text = %q", err)
	}
	if _, statErr := os.Stat(backups); statErr == nil {
		t.Fatal("a refused database must not be touched or backed up")
	}
	if got := versions(t, db); len(got) != 2 {
		t.Fatalf("versions changed to %v", got)
	}
}

func TestApplyRollsBackFailingMigration(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	opts := fixture("bad_sql")
	opts.BackupDir = filepath.Join(dir, "backups")
	_, err := sqlmigrate.Apply(context.Background(), db, opts)
	if err == nil || !strings.Contains(err.Error(), "0002_broken") {
		t.Fatalf("err = %v, want failure naming 0002_broken", err)
	}
	// One transaction: version 1 and the half-applied 0002 are both gone.
	if tableExists(t, db, "items") || tableExists(t, db, "half") {
		t.Fatal("tables survived a failed migration run")
	}
	if got := versions(t, db); len(got) != 0 {
		t.Fatalf("versions = %v, want none", got)
	}
}

func TestApplyFailureAfterBackupKeepsOldSchema(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	if _, err := sqlmigrate.Apply(context.Background(), db, fixture("v1")); err != nil {
		t.Fatal(err)
	}
	opts := fixture("bad_sql")
	opts.BackupDir = filepath.Join(dir, "backups")
	res, err := sqlmigrate.Apply(context.Background(), db, opts)
	if err == nil {
		t.Fatal("want error")
	}
	if res.Backup != "" {
		t.Fatalf("result must be zero on error, got %+v", res)
	}
	ents, _ := os.ReadDir(opts.BackupDir)
	if len(ents) != 1 {
		t.Fatalf("backups = %d, want 1 (taken before the failed run)", len(ents))
	}
	if tableExists(t, db, "half") {
		t.Fatal("half-applied migration survived")
	}
	if got := versions(t, db); len(got) != 1 || got[0] != 1 {
		t.Fatalf("versions = %v", got)
	}
}

func TestLoadValidation(t *testing.T) {
	if _, err := sqlmigrate.Load(os.DirFS("testdata"), "bad_dup"); err == nil || !strings.Contains(err.Error(), "version 1 is used by both") {
		t.Fatalf("duplicate version err = %v", err)
	}
	if _, err := sqlmigrate.Load(os.DirFS("testdata"), "bad_name"); err == nil || !strings.Contains(err.Error(), "NNNN_name.sql") {
		t.Fatalf("bad name err = %v", err)
	}
	if _, err := sqlmigrate.Load(os.DirFS("testdata"), "missing"); err == nil {
		t.Fatal("missing directory must fail")
	}
	if _, err := sqlmigrate.Load(nil, ""); err == nil {
		t.Fatal("nil FS must fail")
	}
	migs, err := sqlmigrate.Load(os.DirFS("testdata"), "good")
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 2 || migs[0].Version != 1 || migs[0].Name != "create_items" || migs[1].Version != 2 {
		t.Fatalf("migrations = %+v", migs)
	}
}

func TestLoadSortsNumericallyAndIgnoresOtherFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"m/0010_ten.sql":   {Data: []byte("SELECT 1;")},
		"m/0002_two.sql":   {Data: []byte("SELECT 1;")},
		"m/README.md":      {Data: []byte("x")},
		"m/sub/0003_x.sql": {Data: []byte("SELECT 1;")},
	}
	migs, err := sqlmigrate.Load(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 2 || migs[0].Version != 2 || migs[1].Version != 10 {
		t.Fatalf("migrations = %+v", migs)
	}
}

func TestApplyEmptyMigrationSet(t *testing.T) {
	db := openDB(t, t.TempDir())
	res, err := sqlmigrate.Apply(context.Background(), db, sqlmigrate.Options{FS: fstest.MapFS{"x.txt": {}}})
	if err != nil || res.Current != 0 || len(res.Applied) != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
