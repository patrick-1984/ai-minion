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

	_ "modernc.org/sqlite" // the SQLite driver; only the store packages import it (AD-1)
)

// Names inside the data directory.
const (
	// DBFile is the Hub database file.
	DBFile = "minion.db"
)

// busyTimeoutMS is how long a statement waits for the write lock.
const busyTimeoutMS = 5000

// Store is the open Hub database: one *sql.DB plus the column encryption
// key. Domain files of this package hang their methods on it.
type Store struct {
	db  *sql.DB
	box *SecretBox
	dir string
}

// Option changes how [Open] works. The defaults are what the Hub uses.
type Option func(*openConfig)

type openConfig struct {
	migrations migrationSet
	clock      Clock
	// afterSnapshot runs in Backup between the database snapshot and the
	// capture of the identity key (tests only).
	afterSnapshot func()
	// passphrase and hasPassphrase select age encryption of a backup or the
	// passphrase a restore tries on an encrypted archive.
	passphrase    string
	hasPassphrase bool
	// maxRestoreBytes is the Restore size limit; 0 means the default.
	maxRestoreBytes int64
}

// WithMigrations replaces the embedded Hub migrations with the NNNN_name.sql
// files in dir of fsys. It exists for tests.
func WithMigrations(fsys fs.FS, dir string) Option {
	return func(c *openConfig) { c.migrations = migrationSet{fsys: fsys, dir: dir} }
}

// WithRestoreLimit sets the limit of [Restore] on the decrypted size of an
// archive and on the total size of the files it declares, in bytes. The
// default is [DefaultMaxRestoreBytes]; a value of 0 or less keeps the default.
func WithRestoreLimit(n int64) Option {
	return func(c *openConfig) { c.maxRestoreBytes = n }
}

// WithClock sets the clock that names backups. It exists for tests.
func WithClock(c Clock) Option {
	return func(cfg *openConfig) { cfg.clock = c }
}

// WithBackupSnapshotHook makes [Backup] call fn after it copied the database
// and before it captures identity.key, the moment at which a rotation of the
// Hub identity would split the two. It exists for tests.
func WithBackupSnapshotHook(fn func()) Option {
	return func(cfg *openConfig) { cfg.afterSnapshot = fn }
}

// Open opens the Hub database in the data directory dir, creating the
// directory (mode 0700), <dir>/data.key and <dir>/minion.db (mode 0600, also
// in a data directory that already existed with a wider mode) on first start.
// An existing database without its data.key is refused: a new key could not
// decrypt the old data.
//
// The database is one *sql.DB with WAL, busy_timeout=5000, _txlock=immediate
// and foreign keys enforced. Pending embedded migrations are applied in one
// transaction after a VACUUM INTO backup in <dir>/backups; a database whose
// schema is newer than this binary is refused with an error that wraps
// [sqlmigrate.ErrSchemaTooNew] (code schema_too_new).
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
		return nil, errors.New("store: data directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: data directory %q: %w", dir, err)
	}
	if err := ensureWritableDir(abs); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(abs, DBFile)
	keyPath := filepath.Join(abs, DataKeyFile)
	if err := refuseKeyReplacement(dbPath, keyPath); err != nil {
		return nil, err
	}
	key, err := LoadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	box, err := NewSecretBox(key)
	if err != nil {
		return nil, err
	}

	if err := createOwnerOnly(dbPath); err != nil {
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
	st := &Store{db: db, box: box, dir: abs}
	if _, err := st.suspendUnusableIdPAdmins(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: reconcile IdP admin grants: %w", err)
	}
	return st, nil
}

// DB returns the database handle. There is exactly one per Hub.
func (s *Store) DB() *sql.DB { return s.db }

// Box returns the column encryption box that uses <data>/data.key.
func (s *Store) Box() *SecretBox { return s.box }

// Dir returns the absolute data directory.
func (s *Store) Dir() string { return s.dir }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// journalSizeLimit is the size the WAL file is cut back to when it restarts.
const journalSizeLimit = 64 << 20

// dsn builds the modernc.org/sqlite DSN for path. The pragmas apply to every
// connection of the pool; _txlock=immediate makes every transaction take the
// write lock when it begins.
func dsn(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:///C:/...
	}
	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// After a checkpoint restarts the log, the WAL file is cut back to this size
	// instead of keeping the size of its largest burst for ever.
	q.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", journalSizeLimit))
	u := url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}
	return u.String()
}

// ensureWritableDir creates dir and proves that the process can write to it,
// with an actionable message when it cannot (a Docker bind mount owned by
// root, A-14).
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: cannot create the data directory %q: %w; make it writable for the user that runs the Hub", dir, err)
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("store: the data directory %q is not writable: %w; make it writable for the user that runs the Hub (in Docker: chown 65532:65532 on the host directory)", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// createOwnerOnly makes sure path exists with mode 0600 before SQLite opens
// it. SQLite gives the -wal and -shm files the mode of the main file, so all
// three are owner-only even when the data directory already existed with a
// wider mode (a Docker bind mount, A-14) and the umask is 022. Existing
// database, -wal and -shm files (a restore) are tightened to 0600 as well.
func createOwnerOnly(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("store: create database file %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: create database file %q: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		// Also protect the sidecars of a restored or crashed database: SQLite
		// keeps the mode of an existing -wal/-shm file, and the WAL holds
		// database pages.
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("store: protect database file %q: %w", p, err)
			}
		}
	}
	return nil
}

// refuseKeyReplacement stops Open from creating a new data key for a database
// that already exists. A new key would not decrypt the sealed columns of the
// old database, and later writes would mix ciphertext of two keys.
func refuseKeyReplacement(dbPath, keyPath string) error {
	if _, err := os.Stat(keyPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil // key present (or an error that LoadOrCreateKey reports)
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil // no database yet: a first start
	}
	return fmt.Errorf("store: the database %q exists but the data key %q is missing; restore %s from your backup, or move the database away to start a new empty Hub (a new key cannot decrypt the old data)",
		dbPath, keyPath, DataKeyFile)
}
