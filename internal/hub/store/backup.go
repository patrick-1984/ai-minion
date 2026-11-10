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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// ManifestFormat is the version of the archive layout described by
// [Manifest].
const ManifestFormat = 1

// maxManifestSize bounds the manifest that Restore reads into memory.
const maxManifestSize = 1 << 20

// archiveFiles lists, in archive order after the manifest, every file that a
// backup may hold and the restore accepts. Everything else is refused, so an
// archive can never write outside the data directory or plant other files.
var archiveFiles = []string{DataKeyFile, IdentityKeyFile, tlsCertFile, tlsKeyFile, DBFile}

// requiredFiles must be in every archive.
var requiredFiles = []string{DBFile, DataKeyFile}

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

// Backup writes one gzip-compressed tar archive of the Hub data directory
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
	if err = writeArchive(f, m, sources); err != nil {
		_ = f.Close()
		return Manifest{}, err
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

	af, err := os.Open(archive)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: cannot open the archive: %v", ErrBackup, errnoReason(err))
	}
	defer func() { _ = af.Close() }()
	gz, err := gzip.NewReader(bufio.NewReader(af))
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: the file is not a gzip archive: %v", ErrBackup, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	m, err := readManifest(tr)
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
	if err := mkdirAllPrivate(dir); err != nil {
		return RestoreResult{}, fmt.Errorf("%w: cannot create the data directory %q: %v", ErrBackup, dir, errnoReason(err))
	}
	stage, err := mkdirTempPrivate(dir, ".restore-*")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%w: cannot write to the data directory %q: %v", ErrBackup, dir, errnoReason(err))
	}
	// The staging directory is removed unless a failed install could not put
	// every previous file back: it then holds the only copy of those files.
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()

	if err := unpack(tr, m, stage); err != nil {
		return RestoreResult{}, err
	}
	schema, err := checkDatabase(ctx, filepath.Join(stage, DBFile))
	if err != nil {
		return RestoreResult{}, err
	}
	if schema != m.SchemaVersion {
		return RestoreResult{}, fmt.Errorf("%w: the database has schema version %d but the manifest says %d", ErrBackup, schema, m.SchemaVersion)
	}
	if err := checkDataKeyFile(filepath.Join(stage, DataKeyFile)); err != nil {
		return RestoreResult{}, err
	}
	files, err := install(stage, dir, m)
	if err != nil {
		keepStage = errors.Is(err, errRecoveryKept)
		return RestoreResult{}, err
	}
	return RestoreResult{Manifest: m, Dir: dir, Files: files}, nil
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
func readManifest(tr *tar.Reader) (Manifest, error) {
	h, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: the archive is empty or damaged: %v", ErrBackup, err)
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
func unpack(tr *tar.Reader, m Manifest, stage string) error {
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
		if err := extract(tr, e, stage); err != nil {
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
func extract(r io.Reader, e ManifestEntry, stage string) error {
	dst := filepath.Join(stage, filepath.FromSlash(e.Name))
	if err := mkdirAllPrivate(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("%w: restore %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	f, err := createPrivate(dst)
	if err != nil {
		return fmt.Errorf("%w: restore %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, e.Size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%w: restore %s: %v", ErrBackup, e.Name, errnoReason(err))
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
