package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// Identity states of a backup (manifest field identity_state), the bootstrap
// states B0 to B3 of the two-tier Hub identity.
const (
	// IdentityStateNone is B0: no hub_identity row, no identity file.
	IdentityStateNone = "none"
	// IdentityStatePlain is B1: the root is the plain identity.key.
	IdentityStatePlain = "plain"
	// IdentityStateSealedNoCert is B2: sealed root, no certificate yet.
	IdentityStateSealedNoCert = "sealed_no_cert"
	// IdentityStateEstablished is B3: sealed root and a certificate.
	IdentityStateEstablished = "established"
)

// RootRollbackWarning is printed when a restored backup holds a root that may
// have been rotated away (contract section 13).
const RootRollbackWarning = "this backup holds a root that may have been rotated away; Workers that followed the rotation will refuse this Hub"

// maxSnapshotAttempts bounds how often Backup repeats the snapshot because
// the identity files and the database copy did not belong together (a state
// change between the copy and the read of the files).
const maxSnapshotAttempts = 5

// isIdentityArchiveName reports whether name is one of the normalised archive
// names of the identity files.
func isIdentityArchiveName(name string) bool {
	return name == IdentityKeyFile || name == SealedRootFile || name == OpKeyFile
}

// dbIdentityInfo is what a database file says about the Hub identity.
type dbIdentityInfo struct {
	Present     bool
	Pub         []byte
	Fingerprint string
	Sealed      bool
	Cert        []byte
	RootSHA     []byte
	Transitions int
}

// state classifies the row. A row that fits no state is an error naming the
// facts.
func (i dbIdentityInfo) state() (string, error) {
	switch {
	case !i.Present:
		return IdentityStateNone, nil
	case !i.Sealed && i.Cert != nil:
		return "", fmt.Errorf("%w: the database records a certificate but no sealed root", ErrBackup)
	case !i.Sealed:
		return IdentityStatePlain, nil
	case len(i.RootSHA) != sha256.Size:
		return "", fmt.Errorf("%w: the database records a sealed root without the SHA-256 of its file", ErrBackup)
	case i.Cert == nil:
		return IdentityStateSealedNoCert, nil
	}
	return IdentityStateEstablished, nil
}

// readDBIdentity reads the identity facts of the database file at p. A
// database without the hub_identity table (or a row) is state none; one from
// before migration 0021 has no sealed root and no certificate.
func readDBIdentity(ctx context.Context, p string) (dbIdentityInfo, error) {
	var info dbIdentityInfo
	db, err := sql.Open("sqlite", "file:"+urlPath(p)+"?mode=ro")
	if err != nil {
		return info, fmt.Errorf("%w: open database copy: %v", ErrBackup, err)
	}
	defer func() { _ = db.Close() }()
	has := func(q string) (bool, error) {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return false, fmt.Errorf("%w: read the database copy: %v", ErrBackup, err)
		}
		return n > 0, nil
	}
	ok, err := has(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'hub_identity'`)
	if err != nil || !ok {
		return info, err
	}
	err = db.QueryRowContext(ctx, `SELECT pub, fingerprint FROM hub_identity WHERE id = 1`).Scan(&info.Pub, &info.Fingerprint)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return dbIdentityInfo{}, nil
	case err != nil:
		return info, fmt.Errorf("%w: read the Hub identity of the database copy: %v", ErrBackup, err)
	}
	info.Present = true
	if v2, err := has(`SELECT COUNT(*) FROM pragma_table_info('hub_identity') WHERE name = 'root_sealed'`); err != nil {
		return info, err
	} else if v2 {
		var sealed int
		if err := db.QueryRowContext(ctx, `SELECT root_sealed, current_cert, root_file_sha256 FROM hub_identity WHERE id = 1`).
			Scan(&sealed, &info.Cert, &info.RootSHA); err != nil {
			return info, fmt.Errorf("%w: read the Hub identity of the database copy: %v", ErrBackup, err)
		}
		info.Sealed = sealed != 0
		if len(info.Cert) == 0 {
			info.Cert = nil
		}
	}
	if t, err := has(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'hub_key_transitions'`); err != nil {
		return info, err
	} else if t {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hub_key_transitions`).Scan(&info.Transitions); err != nil {
			return info, fmt.Errorf("%w: read the transition records of the database copy: %v", ErrBackup, err)
		}
	}
	return info, nil
}

// certOpPub returns the operational public key of a certificate in the
// database, checking that it belongs to the recorded root.
func certOpPub(info dbIdentityInfo) ([]byte, error) {
	c, err := identity.ParseHubCert(info.Cert)
	if err != nil {
		return nil, fmt.Errorf("%w: the certificate recorded in the database is not valid: %v", ErrBackup, err)
	}
	// Verify checks the root and the signature, not the validity window: an
	// expired certificate stays restorable (it is re-issued after the restore).
	switch err := c.Verify(ed25519.PublicKey(info.Pub)); {
	case errors.Is(err, identity.ErrCertRootMismatch):
		return nil, fmt.Errorf("%w: the certificate recorded in the database is not issued by the recorded root", ErrBackup)
	case err != nil:
		return nil, fmt.Errorf("%w: the certificate recorded in the database does not carry a valid signature of the recorded root", ErrBackup)
	}
	return c.OpPub, nil
}

// expectedIdentityFiles lists the identity archive names that belong to state.
func expectedIdentityFiles(state string) []string {
	switch state {
	case IdentityStatePlain:
		return []string{IdentityKeyFile}
	case IdentityStateSealedNoCert:
		return []string{SealedRootFile}
	case IdentityStateEstablished:
		return []string{SealedRootFile, OpKeyFile}
	}
	return nil
}

// snapshot is the result of takeSnapshot: the database copy and the identity
// files, staged under their archive names, that belong to it.
type snapshot struct {
	schema int
	info   dbIdentityInfo
	state  string
	files  map[string]string // archive name -> staged path (identity files only)
}

// takeSnapshot copies the database into stage and stages the identity files
// that belong to that copy (contract section 13, snapshot rule).
//
// The state is read from the copy, never from which files exist. Each file is
// chosen independently: the plain root by its public key (B1), the sealed
// root by the SHA-256 recorded in the copy among root.key.age and
// root.key.age.next (B2, B3), the operational key by the public key of the
// copy's certificate among op.key and op.key.next (B3). When a selected file
// is not found, the whole snapshot is repeated (a state change ran between
// the copy and the read), at most [maxSnapshotAttempts] times.
func takeSnapshot(ctx context.Context, dir, stage string, hook func()) (snapshot, error) {
	stagedDB := filepath.Join(stage, DBFile)
	var lastFacts []string
	var lastState string
	for attempt := 0; attempt < maxSnapshotAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return snapshot{}, fmt.Errorf("%w: %v", ErrBackup, err)
		}
		_ = os.Remove(stagedDB)
		for _, n := range []string{IdentityKeyFile, SealedRootFile, OpKeyFile} {
			_ = os.Remove(filepath.Join(stage, n))
		}
		schema, err := vacuumCopy(ctx, filepath.Join(dir, DBFile), stagedDB)
		if err != nil {
			return snapshot{}, err
		}
		if hook != nil {
			hook()
		}
		info, err := readDBIdentity(ctx, stagedDB)
		if err != nil {
			return snapshot{}, err
		}
		state, err := info.state()
		if err != nil {
			return snapshot{}, err
		}
		snap := snapshot{schema: schema, info: info, state: state, files: map[string]string{}}
		var facts []string
		switch state {
		case IdentityStatePlain:
			ok, f, err := stageFirstKey(dir, stage, IdentityKeyFile, []string{IdentityKeyFile, PendingIdentityKeyFile}, info.Pub, snap.files)
			if err != nil {
				return snapshot{}, err
			}
			if !ok {
				facts = append(facts, f)
			}
		case IdentityStateSealedNoCert, IdentityStateEstablished:
			ok, f, err := stageSealedRoot(dir, stage, info.RootSHA, snap.files)
			if err != nil {
				return snapshot{}, err
			}
			if !ok {
				facts = append(facts, f)
			}
		}
		if state == IdentityStateEstablished {
			op, err := certOpPub(info)
			if err != nil {
				return snapshot{}, err
			}
			ok, f, err := stageFirstKey(dir, stage, OpKeyFile, []string{OpKeyFile, OpKeyNextFile}, op, snap.files)
			if err != nil {
				return snapshot{}, err
			}
			if !ok {
				facts = append(facts, f)
			}
		}
		if len(facts) == 0 {
			return snap, nil
		}
		lastFacts, lastState = facts, state
	}
	return snapshot{}, fmt.Errorf("%w: the database copy is in state %q but the files that belong to it could not be captured in %d attempts (%s); "+
		"the backup would not restore; check that the Hub identity files belong to this database (restore the missing file from an earlier backup if needed) and run the backup again",
		ErrBackup, lastState, maxSnapshotAttempts, strings.Join(lastFacts, "; "))
}

// stageFirstKey stages, as the archive name arch, the first of the plain key
// files names (in dir) whose public key is want.
func stageFirstKey(dir, stage, arch string, names []string, want []byte, out map[string]string) (bool, string, error) {
	var why []string
	for _, name := range names {
		dst := filepath.Join(stage, arch)
		ok, w, err := stageMatchingKey(filepath.Join(dir, name), dst, want)
		if err != nil {
			return false, "", err
		}
		if ok {
			out[arch] = dst
			return true, "", nil
		}
		if w != "" {
			why = append(why, name+" "+w)
		}
	}
	if len(why) == 0 {
		return false, strings.Join(names, " and ") + " not found", nil
	}
	return false, strings.Join(why, ", "), nil
}

// stageSealedRoot stages the sealed root file whose SHA-256 is want.
func stageSealedRoot(dir, stage string, want []byte, out map[string]string) (bool, string, error) {
	var why []string
	for _, name := range []string{SealedRootFile, SealedRootNextFile} {
		raw, err := readSmall(filepath.Join(dir, name), maxSealedRootSize)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return false, "", fmt.Errorf("%w: cannot read %s: %v", ErrBackup, name, errnoReason(err))
		}
		sum := sha256.Sum256(raw)
		if !bytes.Equal(sum[:], want) {
			why = append(why, name+" is not the sealed root recorded in the database copy")
			continue
		}
		dst := filepath.Join(stage, SealedRootFile)
		if err := identity.WritePrivate(dst, raw); err != nil {
			return false, "", fmt.Errorf("%w: stage %s: %v", ErrBackup, name, errnoReason(err))
		}
		out[SealedRootFile] = dst
		return true, "", nil
	}
	if len(why) == 0 {
		return false, SealedRootFile + " and " + SealedRootNextFile + " not found", nil
	}
	return false, strings.Join(why, ", "), nil
}

// readSmall reads a file of at most max bytes; a larger file is reported as
// too large (it can never be the file that was recorded).
func readSmall(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return b[:0], nil // too large: matches nothing
	}
	return b, nil
}

// validateIdentity checks the identity files of the staged directory against
// the state of its (migrated) database: exactly the files of the state, each
// matching what the database records. It returns the state.
func validateIdentity(dir string, m Manifest, info dbIdentityInfo) (string, error) {
	state, err := info.state()
	if err != nil {
		return "", err
	}
	if m.Format == legacyManifestFormat {
		if state != IdentityStateNone && state != IdentityStatePlain {
			return "", fmt.Errorf("%w: a format-1 archive can only hold no identity or a plain identity.key, but its database is in state %q; nothing was restored", ErrBackup, state)
		}
	} else if m.IdentityState != state {
		return "", fmt.Errorf("%w: the manifest says identity state %q but the database in the archive is in state %q; nothing was restored", ErrBackup, m.IdentityState, state)
	}
	var have []string
	for _, e := range m.Files {
		if isIdentityArchiveName(e.Name) {
			have = append(have, e.Name)
		}
	}
	want := expectedIdentityFiles(state)
	sort.Strings(have)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if strings.Join(have, ",") != strings.Join(w, ",") {
		if state == IdentityStatePlain && len(have) == 0 {
			return "", fmt.Errorf("%w: the database in the archive records a Hub identity, but the archive has no %s; "+
				"the Hub would not start, and a new key would strand every enrolled Worker; nothing was restored", ErrBackup, IdentityKeyFile)
		}
		return "", fmt.Errorf("%w: the archive holds the identity files %v but its database is in state %q, which needs exactly %v; nothing was restored", ErrBackup, have, state, want)
	}
	loadPub := func(name string) ([]byte, error) {
		key, err := identity.LoadKey(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("%w: %s in the archive is not a usable key file; nothing was restored", ErrBackup, name)
		}
		return key.Public(), nil
	}
	switch state {
	case IdentityStatePlain:
		pub, err := loadPub(IdentityKeyFile)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(pub, info.Pub) {
			return "", fmt.Errorf("%w: the %s in the archive is not the Hub identity that its database records; nothing was restored", ErrBackup, IdentityKeyFile)
		}
	case IdentityStateSealedNoCert, IdentityStateEstablished:
		raw, err := readSmall(filepath.Join(dir, SealedRootFile), maxSealedRootSize)
		if err != nil {
			return "", fmt.Errorf("%w: cannot read %s: %v", ErrBackup, SealedRootFile, errnoReason(err))
		}
		sum := sha256.Sum256(raw)
		if !bytes.Equal(sum[:], info.RootSHA) {
			return "", fmt.Errorf("%w: the %s in the archive is not the sealed root that its database records; nothing was restored", ErrBackup, SealedRootFile)
		}
		if state == IdentityStateEstablished {
			op, err := certOpPub(info)
			if err != nil {
				return "", err
			}
			pub, err := loadPub(OpKeyFile)
			if err != nil {
				return "", err
			}
			if !bytes.Equal(pub, op) {
				return "", fmt.Errorf("%w: the %s in the archive is not the operational key of the certificate its database records; nothing was restored", ErrBackup, OpKeyFile)
			}
		}
	}
	if m.Format != legacyManifestFormat {
		if m.RootFingerprint != info.Fingerprint {
			return "", fmt.Errorf("%w: the manifest names another root fingerprint than the database in the archive; nothing was restored", ErrBackup)
		}
		if m.TransitionCount != info.Transitions {
			return "", fmt.Errorf("%w: the manifest counts %d transition records, the database in the archive has %d; nothing was restored", ErrBackup, m.TransitionCount, info.Transitions)
		}
	}
	return state, nil
}

// buildPlan computes the trusted install plan: names, sizes and SHA-256
// values of the files of dir, taken now, in this process.
func buildPlan(dir string, m Manifest) ([]ManifestEntry, error) {
	plan := make([]ManifestEntry, 0, len(m.Files))
	for _, e := range m.Files {
		pe, err := describe(e.Name, filepath.Join(dir, filepath.FromSlash(e.Name)))
		if err != nil {
			return nil, err
		}
		plan = append(plan, pe)
	}
	return plan, nil
}

// verifyPlanned checks that the file rel below root still has the size and
// checksum of the plan entry.
func verifyPlanned(root *os.Root, rel string, e ManifestEntry) error {
	f, err := root.Open(rel)
	if err != nil {
		return fmt.Errorf("%w: staged file %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("%w: staged file %s: %v", ErrBackup, e.Name, errnoReason(err))
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%w: the staged file %s changed after it was verified (it does not match the install plan); nothing was installed from it", ErrBackup, e.Name)
	}
	return nil
}

// existingRootRotatedAway reports whether the Hub database already in dir
// proves that the root of the backup (pub) was rotated away: the root of the
// backup is the old root of a transition record of its transition chain. A
// longer chain alone proves nothing about this root. Any problem reading it
// answers false (the conditional advisory is printed anyway).
func existingRootRotatedAway(ctx context.Context, dir string, pub []byte) bool {
	p := filepath.Join(dir, DBFile)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", "file:"+urlPath(p)+"?mode=ro")
	if err != nil {
		return false
	}
	defer func() { _ = db.Close() }()
	var left int
	if db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hub_key_transitions WHERE old_pub = ?`, pub).Scan(&left) != nil {
		return false
	}
	return left > 0
}

// phase2 is the second restore phase: it copies the verified files into a new
// private directory, migrates the database there, checkpoints and closes it
// (no -wal or -shm file remains), checks its integrity, validates the identity
// files against the state of the migrated database and builds the install
// plan from the transformed files. The verified directory is not modified.
// The caller removes the returned directory.
func phase2(ctx context.Context, verified string, m Manifest, cfg openConfig) (dir string, plan []ManifestEntry, info dbIdentityInfo, state string, err error) {
	tmp, terr := mkdirTempPrivate(os.TempDir(), ".minion-restore-p2-*")
	if terr != nil {
		return "", nil, info, "", fmt.Errorf("%w: create a temporary directory: %v", ErrBackup, errnoReason(terr))
	}
	dir = tmp
	// The cleanup uses tmp: the error returns below set the named dir to "".
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	for _, e := range m.Files {
		in, oerr := os.Open(filepath.Join(verified, filepath.FromSlash(e.Name)))
		if oerr != nil {
			return "", nil, info, "", fmt.Errorf("%w: restore %s: %v", ErrBackup, e.Name, errnoReason(oerr))
		}
		err = writeStaged(ctx, in, e, dir)
		_ = in.Close()
		if err != nil {
			return "", nil, info, "", err
		}
	}
	dbPath := filepath.Join(dir, DBFile)
	if err = migrateAndClose(ctx, dbPath, cfg); err != nil {
		return "", nil, info, "", err
	}
	if _, err = checkDatabase(ctx, dbPath); err != nil {
		return "", nil, info, "", err
	}
	if info, err = readDBIdentity(ctx, dbPath); err != nil {
		return "", nil, info, "", err
	}
	if state, err = validateIdentity(dir, m, info); err != nil {
		return "", nil, info, "", err
	}
	if plan, err = buildPlan(dir, m); err != nil {
		return "", nil, info, "", err
	}
	return dir, plan, info, state, nil
}

// migrateAndClose applies the pending migrations to the database file p,
// moves the journal into the main file and closes it, so that no -wal or -shm
// file is left next to it.
func migrateAndClose(ctx context.Context, p string, cfg openConfig) error {
	db, err := sql.Open("sqlite", dsn(p))
	if err != nil {
		return fmt.Errorf("%w: open the database of the archive: %v", ErrBackup, err)
	}
	db.SetMaxOpenConns(1)
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()
	if _, err := migrate(ctx, db, "", cfg.migrations, cfg.clock); err != nil {
		return fmt.Errorf("%w: migrate the database of the archive: %w", ErrBackup, err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("%w: checkpoint the database of the archive: %v", ErrBackup, err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
		return fmt.Errorf("%w: close the journal of the database of the archive: %v", ErrBackup, err)
	}
	closed = true
	if err := db.Close(); err != nil {
		return fmt.Errorf("%w: close the database of the archive: %v", ErrBackup, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(p + suffix); err == nil {
			return fmt.Errorf("%w: the database of the archive left a %s file after it was closed", ErrBackup, suffix)
		}
	}
	return nil
}
