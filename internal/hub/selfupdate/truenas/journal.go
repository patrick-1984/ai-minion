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
	Version           int       `json:"version"`
	ID                string    `json:"id"`
	App               string    `json:"app"`
	Service           string    `json:"service"`
	Phase             string    `json:"phase"`
	Reason            string    `json:"reason,omitempty"`
	RequestID         string    `json:"request_id,omitempty"`
	FromTag           string    `json:"from_tag"`
	ToTag             string    `json:"to_tag"`
	PreviousRef       string    `json:"previous_ref"`
	CandidateRef      string    `json:"candidate_ref"`
	CandidateDigest   string    `json:"candidate_digest"`
	PreviousImageID   string    `json:"previous_image_id"`
	CandidateImageID  string    `json:"candidate_image_id"`
	Dataset           string    `json:"dataset"`
	Snapshot          string    `json:"snapshot,omitempty"`
	ComposeBeforeHash string    `json:"compose_before_hash"`
	ComposeAfterHash  string    `json:"compose_after_hash,omitempty"`
	CandidateStarted  bool      `json:"candidate_started"`
	Jobs              []JobRef  `json:"jobs,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	UpdatedAt         time.Time `json:"updated_at"`
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

// Journal is the per-app journal directory.
type Journal struct {
	dir string
	now func() time.Time
}

// OpenJournal opens (and creates) stateDir/apps/<app>. The state directory
// must be a real directory owned by ownerUID (0 in production, negative in
// tests) and closed to group and others.
func OpenJournal(stateDir, app string, ownerUID int, now func() time.Time) (*Journal, error) {
	if !appRE.MatchString(app) {
		return nil, errors.New("journal: invalid app name")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	for _, d := range []string{stateDir, filepath.Join(stateDir, "apps"), filepath.Join(stateDir, "apps", app)} {
		if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("journal: %w", err)
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
	if now == nil {
		now = time.Now
	}
	return &Journal{dir: filepath.Join(stateDir, "apps", app), now: now}, nil
}

// Lock takes the per-app lock without waiting. The returned function releases
// it; the lock also ends with the process.
func (j *Journal) Lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(j.dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
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

// SaveCompose stores the compose document that is restored on failure. It
// may hold secrets: the file is 0600 inside the root-owned journal.
func (j *Journal) SaveCompose(c Compose) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(j.dir, composeFile), data, 0o600)
}

// LoadCompose reads the saved compose document.
func (j *Journal) LoadCompose() (Compose, error) {
	var c Compose
	ok, err := j.load(composeFile, &c, maxComposeFile)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("journal: the saved compose document is missing")
	}
	return c, nil
}

// DropCompose removes the saved compose document once the attempt ended.
func (j *Journal) DropCompose() { _ = os.Remove(filepath.Join(j.dir, composeFile)) }

func (j *Journal) save(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(j.dir, name), data, 0o600)
}

func (j *Journal) load(name string, v any, limit int64) (bool, error) {
	f, err := openNoFollow(filepath.Join(j.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = f.Close() }()
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
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
