package truenas

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ErrBusy means another run holds the per-app lock.
var ErrBusy = errors.New("another hub-updater run holds the lock")

// Phases of an attempt. The phase is written BEFORE the side effect it names,
// so a killed run leaves the journal at "about to do X" and the next tick
// decides from the real state of the app, the middleware and ZFS.
const (
	PhasePrepared        = "prepared"
	PhaseStopping        = "stopping"
	PhaseSnapshotting    = "snapshotting"
	PhaseUpdating        = "updating"
	PhaseStarting        = "starting"
	PhaseVerifying       = "verifying"
	PhaseFinalizing      = "finalizing"
	PhaseRestoreStop     = "restore_stop"
	PhaseRestoreRollback = "restore_rollback"
	PhaseRestoreUpdate   = "restore_update"
	PhaseRestoreStart    = "restore_start"
	PhaseRestoreVerify   = "restore_verify"

	// Terminal phases.
	PhaseCommitted        = "committed"
	PhaseRolledBack       = "rolled_back"
	PhaseAborted          = "aborted"
	PhaseRecoveryRequired = "recovery_required"
)

// Terminal reports whether the phase ends an attempt.
func Terminal(phase string) bool {
	switch phase {
	case PhaseCommitted, PhaseRolledBack, PhaseAborted, PhaseRecoveryRequired:
		return true
	}
	return false
}

func restorePhase(phase string) bool {
	switch phase {
	case PhaseRestoreStop, PhaseRestoreRollback, PhaseRestoreUpdate, PhaseRestoreStart, PhaseRestoreVerify:
		return true
	}
	return false
}

// JobRef correlates a middleware job with the attempt.
type JobRef struct {
	Op   string `json:"op"`
	ID   int    `json:"id"`
	Done bool   `json:"done"`
}

// Attempt is the durable record of one update attempt. It holds hashes, image
// ids and names, never configuration values (the saved compose document is a
// separate 0600 file).
type Attempt struct {
	Version           int    `json:"version"`
	ID                string `json:"id"`
	App               string `json:"app"`
	Service           string `json:"service"`
	Phase             string `json:"phase"`
	Reason            string `json:"reason,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
	FromTag           string `json:"from_tag"`
	ToTag             string `json:"to_tag"`
	PreviousRef       string `json:"previous_ref"`
	CandidateRef      string `json:"candidate_ref"`
	CandidateDigest   string `json:"candidate_digest"`
	PreviousImageID   string `json:"previous_image_id"`
	CandidateImageID  string `json:"candidate_image_id"`
	Dataset           string `json:"dataset"`
	Snapshot          string `json:"snapshot,omitempty"`
	ComposeBeforeHash string `json:"compose_before_hash"`
	ComposeAfterHash  string `json:"compose_after_hash,omitempty"`
	// SavedHash is the hash of the restore document in compose-before.json
	// (the stored document with the Hub image pinned to the verified
	// previous digest).
	SavedHash string `json:"saved_hash"`
	// DataPath is the host path of the Hub data mount; DatasetGUID the stable
	// identity of the dataset mounted there.
	DataPath    string `json:"data_path"`
	DatasetGUID string `json:"dataset_guid"`
	// Probe describes how the container itself is asked for /readyz.
	ProbeScheme string `json:"probe_scheme"`
	ProbePort   string `json:"probe_port"`
	// Pending is a middleware job about to be submitted: recorded BEFORE the
	// call, so a run killed around the call can find the accepted job.
	Pending          *PendingJob `json:"pending,omitempty"`
	CandidateStarted bool        `json:"candidate_started"`
	Jobs             []JobRef    `json:"jobs,omitempty"`
	StartedAt        time.Time   `json:"started_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

// PendingJob is the intent to submit one middleware job.
type PendingJob struct {
	Op     string    `json:"op"`
	Method string    `json:"method"`
	Since  time.Time `json:"since"`
}

// State is the long-lived record of the executor.
type State struct {
	Version int `json:"version"`
	// LastTag is the release the last committed attempt installed.
	LastTag string `json:"last_tag,omitempty"`
	// Quarantine lists image digests that failed and are not retried.
	Quarantine []string `json:"quarantine,omitempty"`
	// Consumed lists one-shot request ids already used (newest last).
	Consumed []string `json:"consumed,omitempty"`
}

const (
	journalVersion = 1
	maxConsumed    = 64
	attemptFile    = "attempt.json"
	stateFile      = "state.json"
	composeFile    = "compose-before.json"
	lockName       = "lock"
	maxJournalFile = 1 << 20
	maxComposeFile = 4 << 20
)

// Journal is the per-app journal directory. All access goes through an
// os.Root opened once on the validated directory, so a later swap of a path
// component cannot redirect the lock or the records.
type Journal struct {
	root     *os.Root
	ownerUID int
	now      func() time.Time
}

// OpenJournal opens (and creates) stateDir/apps/<app>. With ownerUID >= 0
// (0 in production) the state directory and every ancestor must be a real
// directory owned by ownerUID and closed to group and others (a sticky
// directory is accepted only above the state directory); the journal
// directories must be 0700. A negative ownerUID skips the owner checks (tests).
func OpenJournal(stateDir, app string, ownerUID int, now func() time.Time) (*Journal, error) {
	if !appRE.MatchString(app) {
		return nil, errors.New("journal: invalid app name")
	}
	stateDir = filepath.Clean(stateDir)
	if !filepath.IsAbs(stateDir) {
		return nil, errors.New("journal: state_dir must be absolute")
	}
	if ownerUID >= 0 {
		if err := checkTrustedAncestors(filepath.Dir(stateDir), ownerUID); err != nil {
			return nil, err
		}
	}
	appDir := filepath.Join(stateDir, "apps", app)
	for _, d := range []string{stateDir, filepath.Join(stateDir, "apps"), appDir} {
		if err := os.Mkdir(d, 0o700); err == nil {
			if err := syncDir(filepath.Dir(d)); err != nil {
				return nil, fmt.Errorf("journal: %w", err)
			}
		} else if !errors.Is(err, os.ErrExist) {
			if errors.Is(err, os.ErrNotExist) {
				if err := os.MkdirAll(filepath.Dir(d), 0o700); err != nil {
					return nil, fmt.Errorf("journal: %w", err)
				}
				if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
					return nil, fmt.Errorf("journal: %w", err)
				}
			} else {
				return nil, fmt.Errorf("journal: %w", err)
			}
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("journal: %s is not a plain directory", d)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("journal: %s must not be accessible to group or others", d)
		}
		if err := checkOwner(fi, ownerUID); err != nil {
			return nil, fmt.Errorf("journal: %s: %w", d, err)
		}
	}
	root, err := os.OpenRoot(appDir)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return &Journal{root: root, ownerUID: ownerUID, now: now}, nil
}

// Close releases the directory handle.
func (j *Journal) Close() error { return j.root.Close() }

// checkTrustedAncestors requires every existing directory from / down to dir
// to be owned by uid and not writable by group or others (sticky allowed).
func checkTrustedAncestors(dir string, uid int) error {
	real := dir
	for {
		r, err := filepath.EvalSymlinks(real)
		if err == nil {
			real = r
			break
		}
		parent := filepath.Dir(real)
		if parent == real {
			return fmt.Errorf("journal: cannot resolve %s", dir)
		}
		real = parent
	}
	for p := real; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("journal: %s is not a directory", p)
		}
		if err := checkOwner(fi, uid); err != nil {
			return fmt.Errorf("journal: ancestor %s: %w", p, err)
		}
		if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("journal: ancestor %s is writable by group or others", p)
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Lock takes the per-app lock without waiting. The returned function releases
// it; the lock also ends with the process.
func (j *Journal) Lock() (func(), error) {
	f, err := j.root.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// NewAttemptID returns a random attempt id.
func NewAttemptID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// LoadAttempt returns the last attempt, or nil when there is none.
func (j *Journal) LoadAttempt() (*Attempt, error) {
	var a Attempt
	ok, err := j.load(attemptFile, &a, maxJournalFile)
	if err != nil || !ok {
		return nil, err
	}
	return &a, nil
}

// SaveAttempt writes the attempt atomically and durably.
func (j *Journal) SaveAttempt(a *Attempt) error {
	a.Version = journalVersion
	a.UpdatedAt = j.now().UTC()
	return j.save(attemptFile, a)
}

// LoadState returns the state (zero value when none exists).
func (j *Journal) LoadState() (State, error) {
	var s State
	if _, err := j.load(stateFile, &s, maxJournalFile); err != nil {
		return State{}, err
	}
	return s, nil
}

// SaveState writes the state atomically.
func (j *Journal) SaveState(s State) error {
	s.Version = journalVersion
	if len(s.Consumed) > maxConsumed {
		s.Consumed = s.Consumed[len(s.Consumed)-maxConsumed:]
	}
	return j.save(stateFile, s)
}

// SaveCompose stores the document that is restored on failure. It may hold
// secrets: the file is 0600 inside the root-owned journal.
func (j *Journal) SaveCompose(c Compose) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return j.write(composeFile, data)
}

// LoadCompose reads the saved document and checks it against the hash the
// attempt recorded.
func (j *Journal) LoadCompose(wantHash string) (Compose, error) {
	var c Compose
	ok, err := j.load(composeFile, &c, maxComposeFile)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("journal: the saved compose document is missing")
	}
	if c.Hash() != wantHash {
		return nil, errors.New("journal: the saved compose document does not match its recorded hash")
	}
	return c, nil
}

// DropCompose removes the saved compose document once the attempt ended.
func (j *Journal) DropCompose() { _ = j.root.Remove(composeFile) }

func (j *Journal) save(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return j.write(name, data)
}

func (j *Journal) load(name string, v any, limit int64) (bool, error) {
	f, err := j.root.OpenFile(name, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("journal: %s is not a private regular file", name)
	}
	if err := checkOwner(fi, j.ownerUID); err != nil {
		return false, fmt.Errorf("journal: %s: %w", name, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return false, fmt.Errorf("journal: %w", err)
	}
	if int64(len(data)) > limit {
		return false, fmt.Errorf("journal: %s is too large", name)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("journal: %s is damaged: %w", name, err)
	}
	return true, nil
}

// write replaces a journal file atomically and durably. Every error,
// including the directory sync, fails the call: the caller must not start a
// side effect whose intent is not on disk.
func (j *Journal) write(name string, data []byte) error {
	tmp := ".tmp-" + NewAttemptID()
	f, err := j.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = j.root.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := j.root.Rename(tmp, name); err != nil {
		return err
	}
	ok = true
	d, err := j.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// writeFileAtomic writes data to a temporary file in the same directory,
// syncs it, renames it over path and syncs the directory.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}
