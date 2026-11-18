package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// PauseFile is the file in the state directory that holds the pause of the
// Worker's Hub traffic (tray tickets v3, C3/W3a). It lives outside worker.db,
// so no schema migration is needed and a database restore of an update
// rollback cannot erase it.
const PauseFile = "pause.json"

// PauseState is the content of [PauseFile]. Every change bumps Generation, so
// that a timer of an earlier pause never ends a later one.
type PauseState struct {
	Paused bool `json:"paused"`
	// Until ends a timed pause; zero means until resumed.
	Until time.Time `json:"until,omitzero"`
	// Generation counts the changes; it never goes down.
	Generation uint64 `json:"generation"`
	// SetAt is when the state was written.
	SetAt time.Time `json:"set_at,omitzero"`
}

// Active reports whether the pause is in effect at now: paused and, for a
// timed pause, not yet over.
func (p PauseState) Active(now time.Time) bool {
	return p.Paused && (p.Until.IsZero() || now.Before(p.Until))
}

// pauseFS are the file operations of the pause file that tests replace to
// inject a write or fsync failure.
var pauseFS = struct {
	sync    func(f *os.File) error
	rename  func(from, to string) error
	syncDir func(dir string) error
}{
	sync:    func(f *os.File) error { return f.Sync() },
	rename:  os.Rename,
	syncDir: syncDirectory,
}

// ReadPause reads the pause of the state directory. A missing file is the
// zero state (not paused, generation 0).
func ReadPause(stateDir string) (PauseState, error) {
	var p PauseState
	b, err := os.ReadFile(filepath.Join(stateDir, PauseFile))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("read the pause file: %w", err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return PauseState{}, fmt.Errorf("the pause file %s is not valid: %w", filepath.Join(stateDir, PauseFile), err)
	}
	return p, nil
}

// SetPause writes a new pause state: paused until until (zero: until
// resumed), or resumed when paused is false. The generation is one more than
// the stored one. The file is owner-only and written durably (temporary file,
// fsync, rename, fsync of the directory): when it returns nil the state
// survives a crash, and on any error the change must not be acknowledged.
// The caller serializes writers (the CLI holds the config lock, the Worker
// its pause mutex).
func SetPause(stateDir string, paused bool, until, now time.Time) (PauseState, error) {
	cur, err := ReadPause(stateDir)
	if err != nil {
		return PauseState{}, err
	}
	next := PauseState{Paused: paused, Generation: cur.Generation + 1, SetAt: now.UTC()}
	if paused && !until.IsZero() {
		next.Until = until.UTC()
	}
	if err := writePauseFile(stateDir, next); err != nil {
		return PauseState{}, err
	}
	return next, nil
}

// ClearPause removes the pause file (worker reset and leave). A missing file
// is fine.
func ClearPause(stateDir string) (removed bool, err error) {
	switch err := os.Remove(filepath.Join(stateDir, PauseFile)); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("remove the pause file: %w", err)
	}
}

func writePauseFile(stateDir string, p PauseState) (err error) {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create the state directory: %w", err)
	}
	// CreateTemp creates the file owner-only (0600).
	f, err := os.CreateTemp(stateDir, ".pause-*.tmp")
	if err != nil {
		return fmt.Errorf("write the pause file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write the pause file: %w", err)
	}
	if err := pauseFS.sync(f); err != nil {
		return fmt.Errorf("write the pause file (fsync): %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write the pause file: %w", err)
	}
	if err := pauseFS.rename(tmp, filepath.Join(stateDir, PauseFile)); err != nil {
		return fmt.Errorf("write the pause file: %w", err)
	}
	if err := pauseFS.syncDir(stateDir); err != nil {
		return fmt.Errorf("write the pause file (fsync of the directory): %w", err)
	}
	return nil
}

// syncDirectory makes a rename in dir durable. Windows cannot open a directory
// for a flush; NTFS journals the rename, so it is a no-op there.
func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Local ops of pause and resume (served from tray ticket W3b on). A client
// sends them capability-qualified with [FeaturePause], so that a Worker
// without pause support refuses them as unsupported before anything happens.
const (
	OpLocalPause  = "worker_pause"
	OpLocalResume = "worker_resume"
	// FeaturePause is the local capability of pause and resume.
	FeaturePause = "pause.v1"
)

// LocalPauseArgs are the arguments of [OpLocalPause] and [OpLocalResume]:
// the instance that answered worker_status, and for a timed pause its end
// (RFC 3339). The reply is the new [PauseState].
type LocalPauseArgs struct {
	Instance string `json:"instance"`
	Until    string `json:"until,omitempty"`
}
