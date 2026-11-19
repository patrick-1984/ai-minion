package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the SQLite driver; only the store packages import it (AD-1)

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

// DBFile is the name of the Worker database inside the state directory.
const DBFile = "worker.db"

// busyTimeoutMS is how long a statement waits for the write lock.
const busyTimeoutMS = 5000

// Clock returns the current time. It is a plain function so that tests can
// fix it; nil means [time.Now].
type Clock func() time.Time

func (c Clock) now() time.Time {
	if c == nil {
		return time.Now()
	}
	return c()
}

// Store is the open Worker database: one *sql.DB. Domain files of this
// package hang their methods on it.
type Store struct {
	db    *sql.DB
	dir   string
	clock Clock
}

// Option changes how [Open] works. The defaults are what the Worker uses.
type Option func(*openConfig)

type openConfig struct {
	migrations migrationSet
	clock      Clock
	noMigrate  bool
}

// ErrSchemaMismatch is the error of an open with [NoMigrate] whose database
// has another schema than this binary: older or newer. Nothing was written.
var ErrSchemaMismatch = errors.New("store: the Worker database has another schema than this minion")

// NoMigrate opens an existing database only when its schema is exactly the
// one this binary embeds, in both directions, and never migrates it (tray
// tickets v3, C3): a command of a newer (or older) minion must not move the
// database of a stopped Worker out of that Worker's schema. A mismatch is
// [ErrSchemaMismatch] before anything is written. A state directory without
// a database gets a new one at this binary's schema.
func NoMigrate() Option {
	return func(c *openConfig) { c.noMigrate = true }
}

// WithMigrations replaces the embedded Worker migrations with the
// NNNN_name.sql files in dir of fsys. It exists for tests.
func WithMigrations(fsys fs.FS, dir string) Option {
	return func(c *openConfig) { c.migrations = migrationSet{fsys: fsys, dir: dir} }
}

// WithClock sets the clock that names backups and stamps rows. It exists for
// tests.
func WithClock(c Clock) Option {
	return func(cfg *openConfig) { cfg.clock = c }
}

// Open opens the Worker database <state>/worker.db, creating the state
// directory (mode 0700) and the file (mode 0600, through
// [identity.WritePrivate], which on Windows grants the owner SID and SYSTEM
// only) on first start. An existing database is tightened to 0600 on Unix.
// On Windows the state directory, whether Open created it or it already
// existed, gets a protected DACL with inheritable full-control ACEs for the
// owner SID and SYSTEM only, so the -wal and -shm files (which SQLite creates
// with default security attributes), backups/ and the backup files are private
// too.
//
// The database is one *sql.DB with WAL, busy_timeout=5000 and
// _txlock=immediate. Pending embedded migrations are applied in one
// transaction after a VACUUM INTO backup in <state>/backups (skipped for a new
// empty database); a database whose schema is newer than this binary is
// refused with an error that wraps [sqlmigrate.ErrSchemaTooNew] (code
// schema_too_new).
//
// One Worker process owns a state directory; Open does not guard against two
// processes creating the very first database at the same moment.
func Open(dir string, opts ...Option) (*Store, error) {
	return OpenContext(context.Background(), dir, opts...)
}

// OpenContext is [Open] with a context for the connection and the migrations.
func OpenContext(ctx context.Context, dir string, opts ...Option) (*Store, error) {
	cfg := openConfig{migrations: embeddedMigrations()}
	for _, o := range opts {
		o(&cfg)
	}
	if dir == "" {
		return nil, errors.New("store: state directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: state directory %q: %w", dir, err)
	}
	dbPath := filepath.Join(abs, DBFile)
	if cfg.noMigrate {
		return openNoMigrate(ctx, abs, dbPath, cfg)
	}
	if err := prepareStateDir(abs); err != nil {
		return nil, err
	}
	if err := ensureOwnerOnly(dbPath); err != nil {
		return nil, err
	}
	db, err := openRW(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := migrate(ctx, db, abs, cfg.migrations, cfg.clock); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate database: %w", err)
	}
	return &Store{db: db, dir: abs, clock: cfg.clock}, nil
}

// protectDir is [protectStateDir]; tests replace it to see whether it ran.
var protectDir = protectStateDir

// testHookNoMigrateFresh runs in a [NoMigrate] open of a state directory
// without a database, after the new database was built aside and before it
// is linked into place. It exists for tests.
var testHookNoMigrateFresh func()

// prepareStateDir creates the state directory (mode 0700) and protects it.
func prepareStateDir(abs string) error {
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return fmt.Errorf("store: cannot create the state directory %q: %w; make it writable for the user that runs the Worker", abs, err)
	}
	return protectDir(abs)
}

// openRW opens the database at path for reading and writing (WAL, busy
// timeout, immediate transactions) and checks the connection.
func openRW(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	if err := pingWithBusyRetry(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open database %q: %w", path, err)
	}
	return db, nil
}

// openNoMigrate is the [NoMigrate] open. It never runs a migration on a file
// that already exists at dbPath, whoever created it: an existing database is
// opened only when its applied migrations are exactly this binary's, checked
// read-only before anything (the state directory's ACL included) is written
// and again on the connection the caller writes through. A state directory
// without a database gets a new one built aside under a private temporary
// name and linked into place only when no worker.db appeared meanwhile; if an
// (older) Worker created one first, that database is left to the Worker and
// checked like any existing one.
func openNoMigrate(ctx context.Context, abs, dbPath string, cfg openConfig) (*Store, error) {
	if _, err := os.Stat(dbPath); err == nil {
		return openExistingNoMigrate(ctx, abs, dbPath, cfg)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("store: database file %q: %w", dbPath, err)
	}
	if err := prepareStateDir(abs); err != nil {
		return nil, err
	}
	tmp := filepath.Join(abs, fmt.Sprintf(".%s.new-%d-%d", DBFile, os.Getpid(), time.Now().UnixNano()))
	defer func() {
		for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm", tmp + "-journal"} {
			_ = os.Remove(p)
		}
	}()
	if err := identity.WritePrivate(tmp, nil); err != nil {
		return nil, fmt.Errorf("store: create database file %q: %w", tmp, err)
	}
	db, err := openRW(ctx, tmp)
	if err != nil {
		return nil, err
	}
	if _, err := migrate(ctx, db, abs, cfg.migrations, cfg.clock); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate database: %w", err)
	}
	if err := db.Close(); err != nil { // checkpoints the WAL into the file
		return nil, fmt.Errorf("store: close new database: %w", err)
	}
	if testHookNoMigrateFresh != nil {
		testHookNoMigrateFresh()
	}
	if err := os.Link(tmp, dbPath); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("store: create database file %q: %w", dbPath, err)
	}
	return openExistingNoMigrate(ctx, abs, dbPath, cfg)
}

// openExistingNoMigrate opens the existing database at dbPath without
// migrating it; see [openNoMigrate].
func openExistingNoMigrate(ctx context.Context, abs, dbPath string, cfg openConfig) (*Store, error) {
	if err := checkSameSchema(ctx, abs, cfg.migrations); err != nil {
		return nil, err
	}
	if err := prepareStateDir(abs); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("store: protect database file %q: %w", p, err)
			}
		}
	}
	db, err := openRW(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if err := sameSchema(ctx, db, cfg.migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, dir: abs, clock: cfg.clock}, nil
}

// Connection setup includes journal_mode(WAL). Against a rollback-journal
// writer SQLite can reject that lock upgrade immediately, bypassing its busy
// handler. Retry only BUSY, on a fresh connection (database/sql discards failed
// opens), and bound the whole retry window by the usual busy timeout and ctx.
func pingWithBusyRetry(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(busyTimeoutMS)*time.Millisecond)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := db.PingContext(ctx)
		var coded interface{ Code() int }
		if !errors.As(err, &coded) || coded.Code()&0xff != 5 { // SQLITE_BUSY, including extended codes
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// DB returns the database handle. There is exactly one per Worker.
func (s *Store) DB() *sql.DB { return s.db }

// Dir returns the absolute state directory.
func (s *Store) Dir() string { return s.dir }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// dsn builds the modernc.org/sqlite DSN for path. The pragmas apply to every
// connection of the pool; _txlock=immediate makes every transaction take the
// write lock when it begins. synchronous=FULL syncs the WAL on every commit,
// so an acknowledged counter increment or share change survives a power
// failure (AD-13: a session number is never reused).
func dsn(path string) string {
	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	q.Add("_pragma", "synchronous(FULL)")
	u := databaseURL(path)
	u.RawQuery = q.Encode()
	return u.String()
}

// ensureOwnerOnly makes sure path exists and is private before SQLite opens
// it. A missing file is created through [identity.WritePrivate] (an empty file
// is a valid new SQLite database); SQLite gives the -wal and -shm files the
// mode of the main file on Unix. An existing database and its sidecars (a
// restore, a crash) are tightened to 0600 on Unix, because the WAL holds
// database pages.
func ensureOwnerOnly(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := identity.WritePrivate(path, nil); err != nil {
			return fmt.Errorf("store: create database file %q: %w", path, err)
		}
	} else if err != nil {
		return fmt.Errorf("store: database file %q: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("store: protect database file %q: %w", p, err)
			}
		}
	}
	return nil
}

func databaseURL(path string) url.URL {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:///C:/...
	}
	return url.URL{Scheme: "file", Path: p}
}

// checkSameSchema refuses the existing database of dir, opened read-only,
// unless its applied migrations are exactly the ones of set.
func checkSameSchema(ctx context.Context, dir string, set migrationSet) error {
	ro, err := OpenReadOnlyContext(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = ro.Close() }()
	return sameSchema(ctx, ro.db, set)
}

// sameSchema returns [ErrSchemaMismatch] unless the applied migrations of db
// are exactly the ones of set. A database without a migration ledger (an
// empty file, or tables an older Worker has not finished creating) is a
// mismatch too: [NoMigrate] never initializes a file it did not create.
func sameSchema(ctx context.Context, db *sql.DB, set migrationSet) error {
	applied, _, err := sqlmigrate.Applied(ctx, db)
	if err != nil {
		return err
	}
	migs, err := sqlmigrate.Load(set.fsys, set.dir)
	if err != nil {
		return err
	}
	want := make([]int, 0, len(migs))
	for _, m := range migs {
		want = append(want, m.Version)
	}
	if !slices.Equal(applied, want) {
		have, known := 0, 0
		if len(applied) > 0 {
			have = applied[len(applied)-1]
		}
		if len(want) > 0 {
			known = want[len(want)-1]
		}
		return fmt.Errorf("%w (the database is at version %d, this minion at %d); nothing was changed. "+
			"Start the Worker (or use the same minion version as the Worker), then retry", ErrSchemaMismatch, have, known)
	}
	return nil
}
