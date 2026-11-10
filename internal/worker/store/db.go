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
	"strings"
	"time"

	_ "modernc.org/sqlite" // the SQLite driver; only the store packages import it (AD-1)

	"github.com/patrick-1984/ai-minion/internal/identity"
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
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("store: cannot create the state directory %q: %w; make it writable for the user that runs the Worker", abs, err)
	}

	if err := protectStateDir(abs); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(abs, DBFile)
	if err := ensureOwnerOnly(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open database %q: %w", dbPath, err)
	}
	if _, err := migrate(ctx, db, abs, cfg.migrations, cfg.clock); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate database: %w", err)
	}
	return &Store{db: db, dir: abs, clock: cfg.clock}, nil
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
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:///C:/...
	}
	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	q.Add("_pragma", "synchronous(FULL)")
	u := url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}
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
