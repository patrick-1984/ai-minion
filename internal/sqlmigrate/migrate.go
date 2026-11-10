package sqlmigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrSchemaTooNew is returned (wrapped) when the database has applied a
// migration that this binary does not contain, that is when the schema is
// newer than the binary. Test with [errors.Is]. The adapter edge maps it to
// proto.CodeSchemaTooNew; this package must not import proto (kernel-edges
// lint rule), so the code constant lives only in internal/proto/codes.go.
var ErrSchemaTooNew = errors.New("schema_too_new: database schema is newer than this binary")

// Options configures [Apply].
type Options struct {
	// FS holds the migration files. Required.
	FS fs.FS
	// Dir is the directory inside FS that holds the NNNN_name.sql files.
	// Empty means the root of FS.
	Dir string
	// BackupDir is where the pre-migration VACUUM INTO copy is written. It is
	// created (mode 0700) when needed. Empty disables the backup.
	BackupDir string
	// BackupPrefix starts the backup file name: <prefix>-<timestamp>.db.
	// Empty means "backup".
	BackupPrefix string
	// Now returns the current time; nil means [time.Now]. It stamps the
	// backup name and the applied_at column.
	Now func() time.Time
}

// Result describes what [Apply] did.
type Result struct {
	// Applied lists the versions applied by this call, in order.
	Applied []int
	// Current is the highest applied version after the call (0 when the
	// migration set is empty).
	Current int
	// Backup is the path of the backup file, empty when none was written.
	Backup string
}

// Migration is one parsed NNNN_name.sql file.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

var fileName = regexp.MustCompile(`^([0-9]{4,})_([a-z0-9][a-z0-9_]*)\.sql$`)

// Load reads and validates the migrations of opts.FS/opts.Dir. File names
// must look like 0001_name.sql (at least four digits, lower-case snake case
// name); versions must be unique and are returned in ascending order. Other
// files and sub directories are ignored.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	if fsys == nil {
		return nil, errors.New("sqlmigrate: no migration file system")
	}
	if dir == "" {
		dir = "."
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("sqlmigrate: read migrations: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("sqlmigrate: migration file %q must be named NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil || v < 1 {
			return nil, fmt.Errorf("sqlmigrate: migration file %q has an invalid version", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("sqlmigrate: migration version %d is used by both %q and %q", v, prev, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("sqlmigrate: read %q: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: v, Name: m[2], SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

const createVersionTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL,
	applied_at INTEGER NOT NULL
)`

// Apply brings the database up to the newest embedded migration.
//
// A database that has applied a version unknown to opts.FS is refused with
// [ErrSchemaTooNew] before anything is written. When migrations are pending
// and the database already holds an applied migration, a VACUUM INTO backup
// is written to opts.BackupDir first (a fresh database has nothing to save).
// The pending migrations then run in one transaction together with their
// schema_migrations rows: on any error the transaction rolls back and the
// schema is unchanged.
//
// Apply must run before the database is used by anything else. For a
// transaction that takes the write lock at once, open the database with
// _txlock=immediate.
func Apply(ctx context.Context, db *sql.DB, opts Options) (Result, error) {
	migs, err := Load(opts.FS, opts.Dir)
	if err != nil {
		return Result{}, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	if _, err := db.ExecContext(ctx, createVersionTable); err != nil {
		return Result{}, fmt.Errorf("sqlmigrate: create version table: %w", err)
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return Result{}, err
	}
	if err := checkNotNewer(applied, migs); err != nil {
		return Result{}, err
	}
	pending := pendingOf(applied, migs)
	res := Result{Current: highest(applied)}
	if len(pending) == 0 {
		return res, nil
	}

	if len(applied) > 0 && opts.BackupDir != "" {
		res.Backup, err = backup(ctx, db, opts, now())
		if err != nil {
			return Result{}, err
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("sqlmigrate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-read inside the transaction: another process may have migrated or
	// upgraded the database since the first read.
	applied, err = appliedVersions(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	if err := checkNotNewer(applied, migs); err != nil {
		return Result{}, err
	}
	pending = pendingOf(applied, migs)
	res.Current = highest(applied)
	for _, m := range pending {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return Result{}, fmt.Errorf("sqlmigrate: apply %04d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, now().UnixMilli()); err != nil {
			return Result{}, fmt.Errorf("sqlmigrate: record %04d_%s: %w", m.Version, m.Name, err)
		}
		res.Applied = append(res.Applied, m.Version)
		res.Current = m.Version
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("sqlmigrate: commit: %w", err)
	}
	return res, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func appliedVersions(ctx context.Context, q querier) (map[int]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("sqlmigrate: read version table: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("sqlmigrate: read version table: %w", err)
		}
		out[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlmigrate: read version table: %w", err)
	}
	return out, nil
}

// checkNotNewer refuses a database with an applied version the binary lacks.
func checkNotNewer(applied map[int]bool, migs []Migration) error {
	known := make(map[int]bool, len(migs))
	for _, m := range migs {
		known[m.Version] = true
	}
	var unknown []int
	for v := range applied {
		if !known[v] {
			unknown = append(unknown, v)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Ints(unknown)
	return fmt.Errorf("%w (database has applied version %d, this binary knows up to %d); upgrade the binary, downgrades are not supported",
		ErrSchemaTooNew, unknown[len(unknown)-1], highestKnown(migs))
}

func highestKnown(migs []Migration) int {
	if len(migs) == 0 {
		return 0
	}
	return migs[len(migs)-1].Version
}

func highest(applied map[int]bool) int {
	h := 0
	for v := range applied {
		h = max(h, v)
	}
	return h
}

func pendingOf(applied map[int]bool, migs []Migration) []Migration {
	var out []Migration
	for _, m := range migs {
		if !applied[m.Version] {
			out = append(out, m)
		}
	}
	return out
}

// backup writes <BackupDir>/<prefix>-<timestamp>.db with VACUUM INTO and
// returns its path. The copy holds the same data as the database, so it is
// owner-only like the database itself.
func backup(ctx context.Context, db *sql.DB, opts Options, t time.Time) (string, error) {
	if err := os.MkdirAll(opts.BackupDir, 0o700); err != nil {
		return "", fmt.Errorf("sqlmigrate: create backup directory: %w", err)
	}
	prefix := opts.BackupPrefix
	if prefix == "" {
		prefix = "backup"
	}
	name := fmt.Sprintf("%s-%s.db", prefix, t.UTC().Format("20060102T150405.000Z"))
	dest, err := filepath.Abs(filepath.Join(opts.BackupDir, name))
	if err != nil {
		return "", fmt.Errorf("sqlmigrate: backup path: %w", err)
	}
	// VACUUM INTO accepts an existing empty file. Creating it first with mode
	// 0600 (O_EXCL, so a pre-planted file or link is refused) keeps the copy
	// owner-only from its first byte, whatever the umask and the mode of an
	// already existing backup directory.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("sqlmigrate: create backup file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("sqlmigrate: create backup file: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("sqlmigrate: pre-migration backup: %w", err)
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("sqlmigrate: protect backup: %w", err)
	}
	return dest, nil
}
