package store

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

// Names of the files of a Hub data directory that a backup carries. The
// identity key (Story 4.3) and the generated TLS material (Story 3.2) are
// optional: a Hub that has not created them yet has nothing to save. The
// names are repeated here because this package must not import the packages
// that own them.
const (
	// IdentityKeyFile is the Hub identity key, <data>/identity.key.
	IdentityKeyFile = "identity.key"
	// ManifestFile is the first entry of a backup archive.
	ManifestFile = "manifest.json"

	tlsDirName  = "tls"
	tlsCertFile = tlsDirName + "/cert.pem"
	tlsKeyFile  = tlsDirName + "/key.pem"
)

// scryptWorkFactor is the age scrypt work factor (2^18) that Backup writes and
// the only one Restore accepts: age's own limit (2^22, about 4 GiB of memory
// and many seconds) would let a crafted archive burn resources before any
// authentication.
const scryptWorkFactor = 18

// DefaultMaxRestoreBytes is the default limit of [Restore] on the decrypted
// archive and on the total size of the files an archive declares (64 GiB, far
// above a real Hub database). [WithRestoreLimit] changes it.
const DefaultMaxRestoreBytes int64 = 64 << 30

// maxTrailingBytes bounds what Restore reads after the last tar entry (tar
// padding) on the way to the end of the gzip stream.
const maxTrailingBytes = 1 << 20

// archiveOverheadAllowance is added to the restore limit to form the
// cumulative budget of the decompressed tar stream: tar headers, padding and
// the manifest are not file content, so they are not part of the limit.
const archiveOverheadAllowance = 4 << 20

// errDecryptedTooLarge reports a decrypted archive over the restore limit.
var errDecryptedTooLarge = errors.New("the decrypted archive is larger than the limit")

// errArchiveBudget reports a decompressed archive stream over its budget.
var errArchiveBudget = errors.New("the decompressed archive is larger than the limit")

// restoreDirsHook, when set by a test, runs after the destination
// directories exist and before anything is copied; an error fails the restore.
var restoreDirsHook func(dir string) error

// maxEntrySize is the largest size an archive entry may declare, for the
// files that cannot be big. The database is bounded by the total limit only.
func maxEntrySize(name string) (max int64, exact bool, limited bool) {
	switch name {
	case DataKeyFile:
		return KeySize, true, true
	case IdentityKeyFile:
		return 4096, false, true // the cap of identity.LoadKey
	case tlsCertFile:
		return 1 << 20, false, true
	case tlsKeyFile:
		return 64 << 10, false, true
	}
	return 0, false, false
}

// ManifestFormat is the version of the archive layout described by
// [Manifest].
const ManifestFormat = 1

// maxManifestSize bounds the manifest that Restore reads into memory.
const maxManifestSize = 1 << 20

// archiveFiles lists, in archive order after the manifest, every file that a
// backup may hold and the restore accepts. Everything else is refused, so an
// archive can never write outside the data directory or plant other files.
//
// This is the one place that names the secret and identity files of a Hub data
// directory. A file that exists is archived as it is (the database last, from
// its VACUUM copy), a missing one is skipped, and Restore, the manifest check
// and the install/rollback logic all iterate over this list. A future key or
// certificate file (the two-tier Hub identity adds root.key, op.key, hub.cert
// and, for an interrupted rotation, op.key.next) is therefore a one-line
// addition here, before DBFile. Only a file whose copy must match the database
// snapshot (identity.key today, see [snapshotWithIdentity]) needs more.
var archiveFiles = []string{DataKeyFile, IdentityKeyFile, tlsCertFile, tlsKeyFile, DBFile}

// requiredFiles must be in every archive.
var requiredFiles = []string{DBFile, DataKeyFile}

// ErrPassphraseRequired is wrapped by the error of a restore of an encrypted
// archive without a passphrase.
var ErrPassphraseRequired = errors.New("the archive is encrypted and needs a passphrase")

// ErrWrongPassphrase is wrapped by the error of a restore of an encrypted
// archive with a passphrase that does not decrypt it.
var ErrWrongPassphrase = errors.New("the passphrase does not decrypt the archive")

// ageMagic starts every binary age file; a gzip archive starts with 0x1f 0x8b.
const ageMagic = "age-encryption.org/v1\n"

// WithBackupPassphrase makes [Backup] encrypt the whole archive with age and
// the scrypt passphrase recipient. Without it Backup writes the plain tar.gz
// of earlier versions; the command line asks the operator for an explicit
// choice (a passphrase file or --unencrypted) before it calls Backup.
func WithBackupPassphrase(passphrase string) Option {
	return func(c *openConfig) { c.passphrase, c.hasPassphrase = passphrase, true }
}

// WithRestorePassphrase sets the passphrase [Restore] uses on an archive
// encrypted by age. A plain archive ignores it.
func WithRestorePassphrase(passphrase string) Option {
	return func(c *openConfig) { c.passphrase, c.hasPassphrase = passphrase, true }
}

// ErrBackup is wrapped by every error that says the archive or the data
// directory cannot be backed up or restored for a reason the operator can fix.
var ErrBackup = errors.New("hub backup")

// ManifestEntry describes one archived file.
type ManifestEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is manifest.json, the first entry of a backup archive. It carries
// no secret.
type Manifest struct {
	// Format is [ManifestFormat].
	Format int `json:"format"`
	// Version is the version of the binary that wrote the archive.
	Version string `json:"version"`
	// SchemaVersion is the highest applied migration of the saved database.
	SchemaVersion int `json:"schema_version"`
	// CreatedAt is the backup time, RFC 3339 UTC.
	CreatedAt string `json:"created_at"`
	// Files lists the other archive entries.
	Files []ManifestEntry `json:"files"`
}

func isArchiveFile(name string) bool {
	for _, f := range archiveFiles {
		if f == name {
			return true
		}
	}
	return false
}

// Backup writes one gzip-compressed tar archive (encrypted with age when
// [WithBackupPassphrase] is given) of the Hub data directory
// dataDir to out (created with mode 0600; an existing file is refused). The
// archive holds manifest.json first, then data.key, identity.key and the
// generated TLS certificate and key when they exist, and minion.db as a
// VACUUM INTO copy that is consistent while the Hub runs. The backups
// directory, WAL files and a user-supplied certificate outside <data>/tls are
// not saved.
//
// version is the version of the running binary, recorded in the manifest.
// [WithClock] fixes the time stamp for tests; other options are ignored.
func Backup(ctx context.Context, dataDir, out, version string, opts ...Option) (m Manifest, err error) {
	cfg := openConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	now := time.Now
	if cfg.clock != nil {
		now = cfg.clock
	}
	if dataDir == "" || out == "" {
		return Manifest{}, fmt.Errorf("%w: data directory and output file are required", ErrBackup)
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: data directory %q: %v", ErrBackup, dataDir, err)
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: output file %q: %v", ErrBackup, out, err)
	}
	for _, name := range requiredFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return Manifest{}, fmt.Errorf("%w: %q is not a Hub data directory (cannot read %s: %v)", ErrBackup, dir, name, errnoReason(err))
		}
	}
	// A backup whose key the Hub could not use must not be published.
	if err := checkDataKeyFile(filepath.Join(dir, DataKeyFile)); err != nil {
		return Manifest{}, err
	}
	if err := mkdirAllPrivate(filepath.Dir(outAbs)); err != nil {
		return Manifest{}, fmt.Errorf("%w: create output directory: %v", ErrBackup, errnoReason(err))
	}

	// The VACUUM copy is staged in a private directory next to the output.
	stage, err := mkdirTempPrivate(filepath.Dir(outAbs), ".minion-backup-*")
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: create staging directory: %v", ErrBackup, errnoReason(err))
	}
	defer func() { _ = os.RemoveAll(stage) }()
	stagedDB := filepath.Join(stage, DBFile)
	schema, stagedIdentity, err := snapshotWithIdentity(ctx, dir, stage, cfg.afterSnapshot)
	if err != nil {
		return Manifest{}, err
	}

	// Source path of every archived file.
	sources := map[string]string{DBFile: stagedDB}
	if stagedIdentity != "" {
		sources[IdentityKeyFile] = stagedIdentity
	}
	for _, name := range archiveFiles {
		if name == DBFile || (name == IdentityKeyFile && stagedIdentity != "") {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		st, err := os.Stat(p)
		switch {
		case err == nil && st.Mode().IsRegular():
			sources[name] = p
		case err == nil, errors.Is(err, os.ErrNotExist):
			// Not a regular file or absent: nothing to save.
		default:
			return Manifest{}, fmt.Errorf("%w: cannot read %s: %v", ErrBackup, name, errnoReason(err))
		}
	}

	m = Manifest{
		Format:        ManifestFormat,
		Version:       version,
		SchemaVersion: schema,
		CreatedAt:     now().UTC().Format(time.RFC3339),
	}
	for _, name := range archiveFiles {
		src, ok := sources[name]
		if !ok {
			continue
		}
		e, err := describe(name, src)
		if err != nil {
			return Manifest{}, err
		}
		m.Files = append(m.Files, e)
	}

	f, err := createPrivate(outAbs)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return Manifest{}, fmt.Errorf("%w: output file %q already exists; choose another name or remove it", ErrBackup, outAbs)
		}
		return Manifest{}, fmt.Errorf("%w: create output file: %v", ErrBackup, errnoReason(err))
	}
	defer func() {
		if err != nil {
			_ = os.Remove(outAbs)
		}
	}()
	var sink io.Writer = f
	var enc io.WriteCloser
	if cfg.hasPassphrase {
		if cfg.passphrase == "" {
			_ = f.Close()
			err = fmt.Errorf("%w: the passphrase is empty", ErrBackup)
			return Manifest{}, err
		}
		rcp, rerr := age.NewScryptRecipient(cfg.passphrase)
		if rerr != nil {
			_ = f.Close()
			err = fmt.Errorf("%w: encrypt: %v", ErrBackup, rerr)
			return Manifest{}, err
		}
		if enc, err = age.Encrypt(f, rcp); err != nil {
			_ = f.Close()
			err = fmt.Errorf("%w: encrypt: %v", ErrBackup, errnoReason(err))
			return Manifest{}, err
		}
		sink = enc
	}
	if err = writeArchive(sink, m, sources); err != nil {
		_ = f.Close()
		return Manifest{}, err
	}
	if enc != nil {
		if err = enc.Close(); err != nil {
			_ = f.Close()
			err = fmt.Errorf("%w: encrypt: %v", ErrBackup, errnoReason(err))
			return Manifest{}, err
		}
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return Manifest{}, fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	if err = f.Close(); err != nil {
		return Manifest{}, fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	return m, nil
}

// PendingIdentityKeyFile is the name of the new Hub identity key between the
// commit of a rotation and the replacement of identity.key (Story 4.9). A
// backup may have to save it as identity.key.
const PendingIdentityKeyFile = IdentityKeyFile + ".next"

// maxSnapshotAttempts bounds how often Backup repeats the snapshot because a
// rotation of the Hub identity ran between the database copy and the capture
// of the key.
const maxSnapshotAttempts = 5

// snapshotWithIdentity copies the database into stage and captures, into
// stage too, the identity key that belongs to that copy; it returns the schema
// version and the staged key. The staged key is empty when the snapshot
// records no identity; the caller then archives identity.key as it is, if any.
//
// The database and identity.key change together only in a rotation, and not
// atomically: the commit comes first, the rename of identity.key.next over
// identity.key afterwards. A backup that read the two at different moments
// would hold a database and a private key that do not belong together, and
// the Hub restored from it would refuse to start. So the key is chosen by
// the snapshot: the one of identity.key and identity.key.next whose public key
// the copied hub_identity row records. When neither matches (a rotation
// finished between the copy and the capture), the snapshot is taken again.
// An archive is never published without a matching key once the database
// records an identity.
func snapshotWithIdentity(ctx context.Context, dir, stage string, hook func()) (schema int, stagedKey string, err error) {
	stagedDB := filepath.Join(stage, DBFile)
	var lastWhy string
	for attempt := 0; attempt < maxSnapshotAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, "", fmt.Errorf("%w: %v", ErrBackup, err)
		}
		_ = os.Remove(stagedDB)
		schema, err = vacuumCopy(ctx, filepath.Join(dir, DBFile), stagedDB)
		if err != nil {
			return 0, "", err
		}
		if hook != nil {
			hook()
		}
		recorded, err := recordedIdentityOf(ctx, stagedDB)
		if err != nil {
			return 0, "", err
		}
		if recorded == nil {
			// No identity recorded yet: the key file, when there is one, is
			// recorded at the next start (a restore of the file alone).
			return schema, "", nil
		}
		for i, name := range []string{IdentityKeyFile, PendingIdentityKeyFile} {
			dst := filepath.Join(stage, fmt.Sprintf("identity-candidate-%d", i))
			ok, why, err := stageMatchingKey(filepath.Join(dir, name), dst, recorded)
			if err != nil {
				return 0, "", err
			}
			if ok {
				return schema, dst, nil
			}
			if why != "" {
				lastWhy = name + " " + why
			}
		}
	}
	if lastWhy == "" {
		lastWhy = IdentityKeyFile + " is missing"
	}
	return 0, "", fmt.Errorf("%w: the database records a Hub identity, but no private key matching the snapshot could be captured (%s); "+
		"the backup would not start when restored; check that the Hub identity key files belong to this database and run the backup again",
		ErrBackup, lastWhy)
}

// recordedIdentityOf returns the public key of the hub_identity row of the
// database file at p, or nil when there is no row (or no such table).
func recordedIdentityOf(ctx context.Context, p string) ([]byte, error) {
	db, err := sql.Open("sqlite", "file:"+urlPath(p)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("%w: open database copy: %v", ErrBackup, err)
	}
	defer func() { _ = db.Close() }()
	var pub []byte
	err = db.QueryRowContext(ctx, `SELECT pub FROM hub_identity WHERE id = 1`).Scan(&pub)
	switch {
	case errors.Is(err, sql.ErrNoRows), err != nil && strings.Contains(err.Error(), "no such table"):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: read the Hub identity of the database copy: %v", ErrBackup, err)
	}
	return pub, nil
}

// stageMatchingKey copies the identity key file src to the private file dst
// and reports whether it holds the key whose public key is recorded. A missing
// or unusable src is not an error (ok is false and why says what is wrong,
// empty when src is absent); dst is removed unless it matches.
func stageMatchingKey(src, dst string, recorded []byte) (ok bool, why string, err error) {
	raw, rerr := os.ReadFile(src)
	switch {
	case errors.Is(rerr, os.ErrNotExist):
		return false, "", nil
	case rerr != nil:
		return false, "", fmt.Errorf("%w: cannot read %s: %v", ErrBackup, filepath.Base(src), errnoReason(rerr))
	}
	if werr := identity.WritePrivate(dst, raw); werr != nil {
		return false, "", fmt.Errorf("%w: stage %s: %v", ErrBackup, filepath.Base(src), errnoReason(werr))
	}
	key, kerr := identity.LoadKey(dst)
	if kerr != nil {
		_ = os.Remove(dst)
		return false, "is not a usable identity key", nil
	}
	if !bytes.Equal(key.Public(), recorded) {
		_ = os.Remove(dst)
		return false, "holds a key that is not the identity recorded in the database snapshot", nil
	}
	return true, "", nil
}

// checkDataKeyFile requires a regular file of exactly [KeySize] bytes, the
// only data key the Hub accepts.
func checkDataKeyFile(p string) error {
	st, err := os.Stat(p)
	switch {
	case err != nil:
		return fmt.Errorf("%w: cannot read %s: %v", ErrBackup, DataKeyFile, errnoReason(err))
	case !st.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrBackup, DataKeyFile)
	case st.Size() != KeySize:
		return fmt.Errorf("%w: %s must be exactly %d bytes, it has %d; the Hub could not use it", ErrBackup, DataKeyFile, KeySize, st.Size())
	}
	return nil
}

// vacuumCopy copies the database at src to dst with VACUUM INTO and returns
// its schema version. src must exist; it is opened like the Hub opens it, but
// no migration runs and nothing is created.
func vacuumCopy(ctx context.Context, src, dst string) (int, error) {
	db, err := sql.Open("sqlite", "file:"+urlPath(src)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, fmt.Errorf("%w: open database: %v", ErrBackup, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return 0, fmt.Errorf("%w: copy database: %v", ErrBackup, err)
	}
	return schemaVersionOf(ctx, dst)
}

// schemaVersionOf returns the highest applied migration of the database file
// at path.
func schemaVersionOf(ctx context.Context, p string) (int, error) {
	db, err := sql.Open("sqlite", "file:"+urlPath(p)+"?mode=ro")
	if err != nil {
		return 0, fmt.Errorf("%w: open database copy: %v", ErrBackup, err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("%w: the database has no schema version (not a Hub database?): %v", ErrBackup, err)
	}
	return v, nil
}

// urlPath escapes a file path for use in a sqlite "file:" URI.
func urlPath(p string) string {
	u := url.URL{Path: filepath.ToSlash(p)}
	s := u.EscapedPath()
	if len(s) == 0 || s[0] != '/' {
		s = "/" + s // Windows drive path
	}
	return s
}

// describe returns the manifest entry of the file at src.
func describe(name, src string) (ManifestEntry, error) {
	f, err := os.Open(src)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("%w: cannot read %s: %v", ErrBackup, name, errnoReason(err))
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("%w: read %s: %v", ErrBackup, name, errnoReason(err))
	}
	if name == DataKeyFile && n != KeySize {
		return ManifestEntry{}, fmt.Errorf("%w: %s must be exactly %d bytes, it has %d; the Hub could not use it", ErrBackup, DataKeyFile, KeySize, n)
	}
	return ManifestEntry{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// writeArchive writes the manifest and then every listed file to w.
func writeArchive(w io.Writer, m Manifest, sources map[string]string) error {
	created, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		created = time.Time{}
	}
	bw := bufio.NewWriter(w)
	gz := gzip.NewWriter(bw)
	tw := tar.NewWriter(gz)

	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode manifest: %v", ErrBackup, err)
	}
	body = append(body, '\n')
	if err := tw.WriteHeader(header(ManifestFile, int64(len(body)), created)); err != nil {
		return fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	for _, e := range m.Files {
		if err := addFile(tw, e, sources[e.Name], created); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	return nil
}

// header is the tar header of every entry: a regular file with mode 0600 and
// no owner information.
func header(name string, size int64, mod time.Time) *tar.Header {
	return &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     size,
		Mode:     0o600,
		ModTime:  mod,
		Format:   tar.FormatPAX,
	}
}

// addFile copies src into the archive and checks that it still has the size
// and checksum recorded in e (a key file replaced while the backup ran must
// not produce an archive that its own manifest rejects).
func addFile(tw *tar.Writer, e ManifestEntry, src string, mod time.Time) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%w: cannot read %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	defer func() { _ = f.Close() }()
	if err := tw.WriteHeader(header(e.Name, e.Size, mod)); err != nil {
		return fmt.Errorf("%w: write archive: %v", ErrBackup, errnoReason(err))
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, e.Size))
	if err != nil {
		return fmt.Errorf("%w: write %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%w: %s changed while the backup ran; run the backup again", ErrBackup, e.Name)
	}
	return nil
}

// errnoReason returns the reason of a file system error without the path
// (the caller names what it was doing).
func errnoReason(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// RestoreResult describes a finished restore.
type RestoreResult struct {
	// Manifest is the manifest of the archive.
	Manifest Manifest
	// Dir is the absolute data directory that was restored.
	Dir string
	// Files lists the restored files, relative to Dir.
	Files []string
	// Encrypted reports that the archive was encrypted with age.
	Encrypted bool
}

// Restore unpacks the backup archive into the data directory dataDir, which
// is created (mode 0700) when it does not exist. The Hub must be stopped.
//
// A non-empty data directory is refused unless force is true; with force the
// files the archive can contain (the database with its WAL files, data.key,
// identity.key and the generated TLS material) are replaced and everything
// else in the directory, such as backups/, is left alone. An archive whose
// schema is newer than this binary is refused with an error that wraps
// [sqlmigrate.ErrSchemaTooNew]. Every file is verified against the manifest
// and the database is integrity-checked in a staging directory before
// anything is moved into place. Restored files are created with mode 0600 and
// belong to the calling user. [WithMigrations] sets the schema the binary
// knows, for tests.
func Restore(ctx context.Context, archive, dataDir string, force bool, opts ...Option) (RestoreResult, error) {
	cfg := openConfig{migrations: embeddedMigrations()}
	for _, o := range opts {
		o(&cfg)
	}
	known, err := knownSchema(cfg.migrations)
	if err != nil {
		return RestoreResult{}, err
	}
	if archive == "" || dataDir == "" {
		return RestoreResult{}, fmt.Errorf("%w: archive and data directory are required", ErrBackup)
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: data directory %q: %v", ErrBackup, dataDir, err)
	}
	limit := cfg.maxRestoreBytes
	if limit <= 0 {
		limit = DefaultMaxRestoreBytes
	}

	af, err := os.Open(archive)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: cannot open the archive: %v", ErrBackup, errnoReason(err))
	}
	defer func() { _ = af.Close() }()
	br := bufio.NewReader(af)
	encrypted := false
	if head, _ := br.Peek(len(ageMagic)); string(head) == ageMagic {
		encrypted = true
		if !cfg.hasPassphrase {
			return RestoreResult{}, fmt.Errorf("%w: %w; pass the passphrase file with --passphrase-file", ErrBackup, ErrPassphraseRequired)
		}
		// Decrypt and authenticate the whole archive into a private temp
		// area first: a wrong passphrase or a truncated file fails here,
		// before the data directory is touched.
		plain, cleanup, derr := decryptToTemp(ctx, br, cfg.passphrase, limit)
		if derr != nil {
			return RestoreResult{}, derr
		}
		defer cleanup()
		br = bufio.NewReader(plain)
	}
	gz, err := gzip.NewReader(br)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: the file is not a gzip archive: %v", ErrBackup, err)
	}
	defer func() { _ = gz.Close() }()
	// Every decompressed byte the tar reader consumes, including the PAX and
	// GNU extension headers that Next reads internally, counts against one
	// cumulative budget and is checked against the context.
	budget := newBudgetReader(ctx, gz, limit)
	tr := tar.NewReader(budget)

	m, err := readManifest(tr, limit)
	if err != nil {
		return RestoreResult{}, err
	}
	if m.SchemaVersion > known {
		return RestoreResult{}, fmt.Errorf("%w: the archive has schema version %d, this binary knows up to %d (archive written by %s); restore it with a newer binary, downgrades are not supported",
			sqlmigrate.ErrSchemaTooNew, m.SchemaVersion, known, m.Version)
	}

	if err := checkTarget(dir, force); err != nil {
		return RestoreResult{}, err
	}

	// Everything is unpacked and validated in a private staging directory
	// outside the destination, which is created only when the archive passed.
	vstage, err := mkdirTempPrivate(os.TempDir(), ".minion-restore-check-*")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: create a temporary directory: %v", ErrBackup, errnoReason(err))
	}
	defer func() { _ = os.RemoveAll(vstage) }()

	if err := unpack(ctx, tr, m, vstage); err != nil {
		return RestoreResult{}, err
	}
	// Read the gzip stream to its verified end (CRC and length trailer).
	if err := drainGzip(ctx, budget); err != nil {
		return RestoreResult{}, err
	}
	schema, err := checkDatabase(ctx, filepath.Join(vstage, DBFile))
	if err != nil {
		return RestoreResult{}, err
	}
	if schema != m.SchemaVersion {
		return RestoreResult{}, fmt.Errorf("%w: the database has schema version %d but the manifest says %d", ErrBackup, schema, m.SchemaVersion)
	}
	if err := checkDataKeyFile(filepath.Join(vstage, DataKeyFile)); err != nil {
		return RestoreResult{}, err
	}
	if err := checkStagedIdentity(ctx, vstage); err != nil {
		return RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, fmt.Errorf("%w: %w", ErrBackup, err)
	}

	// Validation passed: now the destination may be created. The missing
	// directories this call creates are removed again (when still empty, in
	// reverse order) on any failure, unless recovery files are kept in them.
	created := missingDirs(dir)
	stage := ""
	keepStage := false
	ok := false
	defer func() {
		if keepStage {
			return
		}
		if stage != "" {
			_ = os.RemoveAll(stage)
		}
		if ok {
			return
		}
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i]) // only when still empty
		}
	}()
	if err := mkdirAllPrivate(dir); err != nil {
		return RestoreResult{}, fmt.Errorf("%w: cannot create the data directory %q: %v", ErrBackup, dir, errnoReason(err))
	}
	stage, err = mkdirTempPrivate(dir, ".restore-*")
	if err != nil {
		stage = ""
		return RestoreResult{}, fmt.Errorf("%w: cannot write to the data directory %q: %v", ErrBackup, dir, errnoReason(err))
	}
	if restoreDirsHook != nil {
		if err := restoreDirsHook(dir); err != nil {
			return RestoreResult{}, err
		}
	}

	if err = copyStaged(ctx, vstage, stage, m); err != nil {
		return RestoreResult{}, err
	}
	files, err := install(stage, dir, m)
	if err != nil {
		keepStage = errors.Is(err, errRecoveryKept)
		return RestoreResult{}, err
	}
	ok = true
	return RestoreResult{Manifest: m, Dir: dir, Files: files, Encrypted: encrypted}, nil
}

// checkStagedIdentity requires, when the staged database records a Hub
// identity, a valid identity.key in the staged files whose public key is the
// recorded one: the same rule the Hub applies at start, checked before
// anything is installed.
func checkStagedIdentity(ctx context.Context, stage string) error {
	recorded, err := recordedIdentityOf(ctx, filepath.Join(stage, DBFile))
	if err != nil {
		return err
	}
	if recorded == nil {
		return nil
	}
	key, err := identity.LoadKey(filepath.Join(stage, IdentityKeyFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: the database in the archive records a Hub identity, but the archive has no %s; "+
			"the Hub would not start, and a new key would strand every enrolled Worker; nothing was restored", ErrBackup, IdentityKeyFile)
	case err != nil:
		return fmt.Errorf("%w: the database in the archive records a Hub identity, but %s is not a usable identity key; nothing was restored", ErrBackup, IdentityKeyFile)
	case !bytes.Equal(key.Public(), recorded):
		return fmt.Errorf("%w: the %s in the archive is not the Hub identity that its database records; nothing was restored", ErrBackup, IdentityKeyFile)
	}
	return nil
}

// missingDirs returns the directories among dir and its ancestors that do not
// exist yet, outermost first.
func missingDirs(dir string) []string {
	var missing []string
	for p := dir; ; {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
			break
		}
		missing = append([]string{p}, missing...)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return missing
}

// budgetReader limits the cumulative bytes read from r to a budget and fails
// reads once ctx is done.
type budgetReader struct {
	ctx    context.Context
	r      io.Reader
	left   int64
	budget int64
}

func newBudgetReader(ctx context.Context, r io.Reader, limit int64) *budgetReader {
	b := limit + archiveOverheadAllowance
	if b < limit { // overflow
		b = math.MaxInt64
	}
	return &budgetReader{ctx: ctx, r: r, left: b, budget: b}
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.left <= 0 {
		// One more byte decides between exact end and an overrun.
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: %w (%d bytes, see --max-size)", ErrBackup, errArchiveBudget, b.budget)
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// ctxReader fails reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// copyCtx copies at most max bytes from src to dst and reports whether src
// held more; it stops with the context error when ctx is done.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader, max int64) (n int64, tooBig bool, err error) {
	lim := max
	if lim < math.MaxInt64 {
		lim++
	}
	n, err = io.Copy(dst, io.LimitReader(ctxReader{ctx, src}, lim))
	if err == nil && n > max {
		return max, true, nil
	}
	return n, false, err
}

// drainGzip reads the rest of the gzip stream (tar padding) up to
// maxTrailingBytes and requires its verified end, which checks the CRC and
// length trailer of the whole archive.
func drainGzip(ctx context.Context, gz io.Reader) error {
	_, tooBig, err := copyCtx(ctx, io.Discard, gz, maxTrailingBytes)
	switch {
	case err != nil:
		return fmt.Errorf("%w: the archive is damaged (gzip stream): %w", ErrBackup, err)
	case tooBig:
		return fmt.Errorf("%w: the archive has unexpected data after its last entry", ErrBackup)
	}
	return nil
}

// copyStaged copies the validated files from the private check directory into
// the staging directory next to the destination (it may be on another file
// system, so a rename is not enough) and verifies them again.
func copyStaged(ctx context.Context, from, to string, m Manifest) error {
	for _, e := range m.Files {
		if err := copyStagedFile(ctx, filepath.Join(from, filepath.FromSlash(e.Name)), to, e); err != nil {
			return err
		}
	}
	return nil
}

func copyStagedFile(ctx context.Context, src, to string, e ManifestEntry) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%w: restore %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	defer func() { _ = in.Close() }()
	return writeStaged(ctx, in, e, to)
}

// decryptToTemp decrypts the age stream r with passphrase into a file in a new
// private directory below the system temp directory and returns it opened for
// reading, with a cleanup that removes it. age authenticates every chunk
// and the end of the stream, so a nil error means the archive is complete and
// unmodified.
func decryptToTemp(ctx context.Context, r io.Reader, passphrase string, limit int64) (*os.File, func(), error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrBackup, err)
	}
	// Refuse an excessive work factor before the key derivation runs.
	id.SetMaxWorkFactor(scryptWorkFactor)
	dr, err := age.Decrypt(r, id)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, nil, fmt.Errorf("%w: %w", ErrBackup, ErrWrongPassphrase)
		}
		return nil, nil, fmt.Errorf("%w: the encrypted archive cannot be decrypted: %v", ErrBackup, err)
	}
	tmp, err := mkdirTempPrivate(os.TempDir(), ".minion-restore-*")
	if err != nil {
		return nil, nil, fmt.Errorf("%w: create a temporary directory: %v", ErrBackup, errnoReason(err))
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	f, err := createPrivate(filepath.Join(tmp, "archive.tar.gz"))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("%w: create a temporary file: %v", ErrBackup, errnoReason(err))
	}
	if _, tooBig, err := copyCtx(ctx, f, dr, limit); err != nil || tooBig {
		_ = f.Close()
		cleanup()
		if tooBig {
			return nil, nil, fmt.Errorf("%w: %w of %d bytes (see --max-size)", ErrBackup, errDecryptedTooLarge, limit)
		}
		return nil, nil, fmt.Errorf("%w: the encrypted archive is damaged or truncated: %w", ErrBackup, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("%w: write the decrypted archive: %v", ErrBackup, errnoReason(err))
	}
	rf, err := os.Open(f.Name())
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("%w: read the decrypted archive: %v", ErrBackup, errnoReason(err))
	}
	return rf, func() { _ = rf.Close(); cleanup() }, nil
}

// knownSchema returns the highest migration version of set.
func knownSchema(set migrationSet) (int, error) {
	migs, err := sqlmigrate.Load(set.fsys, set.dir)
	if err != nil {
		return 0, err
	}
	if len(migs) == 0 {
		return 0, nil
	}
	return migs[len(migs)-1].Version, nil
}

// readManifest reads and validates the first archive entry.
func readManifest(tr *tar.Reader, limit int64) (Manifest, error) {
	h, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: the archive is empty or damaged: %w", ErrBackup, err)
	}
	if h.Name != ManifestFile || h.Typeflag != tar.TypeReg {
		return Manifest{}, fmt.Errorf("%w: the file is not a Hub backup (first entry is not %s)", ErrBackup, ManifestFile)
	}
	if h.Size > maxManifestSize {
		return Manifest{}, fmt.Errorf("%w: the manifest is too large", ErrBackup)
	}
	body, err := io.ReadAll(io.LimitReader(tr, maxManifestSize))
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: read manifest: %v", ErrBackup, err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: the manifest is not valid: %v", ErrBackup, err)
	}
	if m.Format != ManifestFormat {
		return Manifest{}, fmt.Errorf("%w: unsupported archive format %d (this binary reads format %d)", ErrBackup, m.Format, ManifestFormat)
	}
	if m.SchemaVersion < 0 {
		return Manifest{}, fmt.Errorf("%w: the manifest has a negative schema version", ErrBackup)
	}
	seen := map[string]bool{}
	var total int64
	for _, e := range m.Files {
		switch {
		case !isArchiveFile(e.Name):
			return Manifest{}, fmt.Errorf("%w: the manifest lists the unexpected file %q", ErrBackup, e.Name)
		case seen[e.Name]:
			return Manifest{}, fmt.Errorf("%w: the manifest lists %s twice", ErrBackup, e.Name)
		case e.Size < 0:
			return Manifest{}, fmt.Errorf("%w: the manifest has a negative size for %s", ErrBackup, e.Name)
		}
		if b, err := hex.DecodeString(e.SHA256); err != nil || len(b) != sha256.Size {
			return Manifest{}, fmt.Errorf("%w: the manifest has an invalid checksum for %s", ErrBackup, e.Name)
		}
		max, exact, limited := maxEntrySize(e.Name)
		switch {
		case limited && exact && e.Size != max:
			return Manifest{}, fmt.Errorf("%w: %s must be exactly %d bytes, the manifest says %d; the Hub could not use it", ErrBackup, e.Name, max, e.Size)
		case limited && e.Size > max:
			return Manifest{}, fmt.Errorf("%w: the manifest declares %d bytes for %s, more than the %d it can have", ErrBackup, e.Size, e.Name, max)
		case e.Size > limit-total:
			return Manifest{}, fmt.Errorf("%w: the archive declares more than the limit of %d bytes (see --max-size)", ErrBackup, limit)
		}
		total += e.Size
		seen[e.Name] = true
	}
	for _, name := range requiredFiles {
		if !seen[name] {
			return Manifest{}, fmt.Errorf("%w: the archive does not contain %s", ErrBackup, name)
		}
	}
	return m, nil
}

// checkTarget refuses a target that cannot or may not be restored into.
func checkTarget(dir string, force bool) error {
	st, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%w: cannot read the data directory %q: %v", ErrBackup, dir, errnoReason(err))
	case !st.IsDir():
		return fmt.Errorf("%w: %q is not a directory", ErrBackup, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%w: cannot read the data directory %q: %v", ErrBackup, dir, errnoReason(err))
	}
	if len(entries) > 0 && !force {
		return fmt.Errorf("%w: the data directory %q is not empty; stop the Hub and use --force to replace its database and keys", ErrBackup, dir)
	}
	return nil
}

// unpack extracts every remaining entry of tr into stage, verifying name,
// type, size and checksum against m, and requires that every file of m is
// present.
func unpack(ctx context.Context, tr *tar.Reader, m Manifest, stage string) error {
	want := map[string]ManifestEntry{}
	for _, e := range m.Files {
		want[e.Name] = e
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: the archive is damaged: %v", ErrBackup, err)
		}
		e, ok := want[h.Name]
		if !ok || path.Clean(h.Name) != h.Name {
			return fmt.Errorf("%w: the archive contains the unexpected entry %q", ErrBackup, h.Name)
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%w: the entry %q is not a regular file", ErrBackup, h.Name)
		}
		if h.Size != e.Size {
			return fmt.Errorf("%w: the entry %q has size %d, the manifest says %d", ErrBackup, h.Name, h.Size, e.Size)
		}
		delete(want, h.Name)
		if err := extract(ctx, tr, e, stage); err != nil {
			return err
		}
	}
	if len(want) > 0 {
		names := make([]string, 0, len(want))
		for n := range want {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("%w: the archive is incomplete, missing %v", ErrBackup, names)
	}
	return nil
}

// extract writes one entry to stage (readable by the owner only, created
// exclusively) and verifies its checksum.
func extract(ctx context.Context, r io.Reader, e ManifestEntry, stage string) error {
	return writeStaged(ctx, r, e, stage)
}

// writeStaged copies exactly e.Size bytes of r to the file e.Name below dir
// (private, created exclusively), honouring ctx, and verifies the checksum.
func writeStaged(ctx context.Context, r io.Reader, e ManifestEntry, dir string) error {
	dst := filepath.Join(dir, filepath.FromSlash(e.Name))
	if err := mkdirAllPrivate(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("%w: restore %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	f, err := createPrivate(dst)
	if err != nil {
		return fmt.Errorf("%w: restore %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	h := sha256.New()
	n, _, err := copyCtx(ctx, io.MultiWriter(f, h), r, e.Size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%w: restore %s: %w", ErrBackup, e.Name, errnoReason(err))
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%w: the checksum of %s does not match the manifest; the archive is damaged", ErrBackup, e.Name)
	}
	return nil
}

// checkDatabase runs an integrity check on the staged database and returns
// its schema version.
func checkDatabase(ctx context.Context, p string) (int, error) {
	db, err := sql.Open("sqlite", "file:"+urlPath(p)+"?mode=ro")
	if err != nil {
		return 0, fmt.Errorf("%w: open the restored database: %v", ErrBackup, err)
	}
	defer func() { _ = db.Close() }()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return 0, fmt.Errorf("%w: the database in the archive is damaged: %v", ErrBackup, err)
	}
	if res != "ok" {
		return 0, fmt.Errorf("%w: the database in the archive failed its integrity check", ErrBackup)
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("%w: the database in the archive has no schema version: %v", ErrBackup, err)
	}
	return v, nil
}

// errRecoveryKept marks a failed install that left the previous files in the
// staging directory because rolling back did not complete.
var errRecoveryKept = errors.New("the restore staging directory was kept")

// installRename moves a file inside the confined data directory. Tests
// replace it to inject failures.
var installRename = func(root *os.Root, from, to string) error { return root.Rename(from, to) }

// move is one file moved aside or into place, as root-relative slash paths.
type move struct{ from, to string }

// install moves the staged files into dir. With a non-empty directory the
// files an archive can contain (including the database's WAL and
// shared-memory files, which would corrupt the restored database) are first
// moved aside into the staging directory; if any later step fails, the new
// files are removed and the old ones moved back, so a failed restore leaves
// the previous Hub usable. The old files are deleted with the staging
// directory only after every new file is in place; when a rollback step
// itself fails, the error wraps errRecoveryKept and names the staging
// directory, which then holds the only copy of the files that could not be
// put back and must not be deleted. The database moves last.
//
// All paths are resolved through an [os.Root] on dir: a symlinked tls
// directory or file that points outside dir is refused instead of followed,
// and nothing outside dir is removed or replaced.
func install(stage, dir string, m Manifest) (files []string, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: open the data directory %q: %v", ErrBackup, dir, errnoReason(err))
	}
	defer func() { _ = root.Close() }()
	stageRel := filepath.Base(stage)

	present := map[string]bool{}
	for _, e := range m.Files {
		present[e.Name] = true
	}

	// Refuse everything that cannot be replaced before touching anything.
	if st, lerr := root.Lstat(tlsDirName); lerr == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s in the data directory is a symbolic link; refusing to restore through it", ErrBackup, tlsDirName)
	}
	var aside []move
	for _, name := range archiveFiles {
		for _, suffix := range sidecars(name) {
			rel := name + suffix
			st, lerr := root.Lstat(rel)
			switch {
			case errors.Is(lerr, os.ErrNotExist):
				continue
			case lerr != nil:
				return nil, fmt.Errorf("%w: cannot replace %s: %v", ErrBackup, rel, errnoReason(lerr))
			case st.IsDir():
				return nil, fmt.Errorf("%w: cannot replace %s: it is a directory", ErrBackup, rel)
			}
			aside = append(aside, move{from: rel, to: stageRel + "/old/" + strings.ReplaceAll(rel, "/", "_")})
		}
	}

	var moved []move     // aside moves done, to undo
	var installed []move // new files in place, to undo
	madeTLS, protectedTLS := false, false
	rollback := func(cause error) error {
		var problems []string
		for i := len(installed) - 1; i >= 0; i-- {
			if rerr := root.Remove(installed[i].to); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				problems = append(problems, fmt.Sprintf("remove %s: %v", installed[i].to, errnoReason(rerr)))
			}
		}
		for i := len(moved) - 1; i >= 0; i-- {
			if rerr := installRename(root, moved[i].to, moved[i].from); rerr != nil {
				problems = append(problems, fmt.Sprintf("restore %s: %v", moved[i].from, errnoReason(rerr)))
			}
		}
		if madeTLS {
			_ = root.Remove(tlsDirName) // only when still empty
		}
		if len(problems) > 0 {
			// The staging directory now holds the only copy of the previous
			// files that could not be moved back; Restore must keep it.
			return fmt.Errorf("%w; rolling back also failed (%s); the previous files that could not be put back are kept in %s, move them back into %s by hand (names have / replaced by _) and delete that directory afterwards: %w",
				cause, strings.Join(problems, "; "), filepath.Join(dir, stageRel, "old"), dir, errRecoveryKept)
		}
		return cause
	}
	if len(aside) > 0 {
		if err := root.MkdirAll(stageRel+"/old", 0o700); err != nil {
			return nil, fmt.Errorf("%w: prepare the restore: %v", ErrBackup, errnoReason(err))
		}
	}
	for _, mv := range aside {
		if err := installRename(root, mv.from, mv.to); err != nil {
			return nil, rollback(fmt.Errorf("%w: cannot replace %s: %v", ErrBackup, mv.from, errnoReason(err)))
		}
		moved = append(moved, mv)
	}
	for _, name := range archiveFiles {
		if !present[name] {
			continue
		}
		if parent := path.Dir(name); parent != "." {
			if _, lerr := root.Lstat(parent); errors.Is(lerr, os.ErrNotExist) {
				madeTLS = true
			}
			if err := root.MkdirAll(parent, 0o700); err != nil {
				return nil, rollback(fmt.Errorf("%w: restore %s: %v", ErrBackup, name, errnoReason(err)))
			}
			if madeTLS && !protectedTLS {
				// On Windows a new directory inherits the ACL of the data
				// directory; the key it receives must stay private.
				if err := protectDir(filepath.Join(dir, filepath.FromSlash(parent))); err != nil {
					return nil, rollback(fmt.Errorf("%w: restore %s: %v", ErrBackup, name, errnoReason(err)))
				}
				protectedTLS = true
			}
		}
		mv := move{from: stageRel + "/" + name, to: name}
		if err := installRename(root, mv.from, mv.to); err != nil {
			return nil, rollback(fmt.Errorf("%w: restore %s: %v", ErrBackup, name, errnoReason(err)))
		}
		installed = append(installed, mv)
		files = append(files, name)
	}
	return files, nil
}

// sidecars returns the suffixes of the files that belong to name.
func sidecars(name string) []string {
	if name == DBFile {
		return []string{"", "-wal", "-shm", "-journal"}
	}
	return []string{""}
}
