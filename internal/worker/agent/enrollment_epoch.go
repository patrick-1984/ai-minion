package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// The enrollment generation is kept across processes (Story 4.7).
//
// The Worker service and `minion worker pair` are two processes with their own
// [Enrollments] value over one state directory. A generation that lives only in
// memory would let the service keep an old Hub's connection, and an old Hub's
// signed revoke, after the CLI replaced the enrollment. So the generation is the
// number in the epoch file, and every writer advances it while it holds the
// lock file, before it changes the enrollment. The conditional writers check it
// under the same lock, which makes "check, then write" one step across
// processes. Without a state directory (unit tests of other packages) the
// generation stays in memory.
const (
	// enrollmentEpochFile holds the generation as a decimal number.
	enrollmentEpochFile = "enrollment.epoch"
	// enrollmentLockFile is the file the cross-process lock is taken on. It
	// stays empty and is never replaced, so the lock is never lost to a rename.
	enrollmentLockFile = "enrollment.lock"
	// stateLockWait bounds the wait for the lock: holders keep it for one
	// database write.
	stateLockWait = 30 * time.Second
)

func (e *Enrollments) epochPath() string { return filepath.Join(e.stateDir, enrollmentEpochFile) }

// lockState takes the cross-process lock; it is a no-op without a state
// directory.
func (e *Enrollments) lockState(ctx context.Context) (unlock func(), err error) {
	if e.stateDir == "" {
		return func() {}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, stateLockWait)
	defer cancel()
	unlock, err = lockFile(ctx, filepath.Join(e.stateDir, enrollmentLockFile))
	if err != nil {
		return nil, fmt.Errorf("agent: lock the enrollment state: %w", err)
	}
	return unlock, nil
}

// locked runs fn with the process mutex and the cross-process lock held. A
// generation that fn advanced is announced to the waiters of this process when
// fn is done, not before: an attempt that is dropped by the announcement must
// not see its own context end in the middle of the write that caused it.
func (e *Enrollments) locked(ctx context.Context, fn func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	unlock, err := e.lockState(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	defer e.announceLocked()
	return fn()
}

// lockedIf is locked for the enrollment of generation gen: it runs fn only when
// the generation (in the file, not just in this process) is still gen.
func (e *Enrollments) lockedIf(ctx context.Context, gen uint64, fn func() error) error {
	return e.locked(ctx, func() error {
		if err := e.syncLocked(); err != nil {
			return err
		}
		if e.gen != gen {
			return ErrEnrollmentChanged
		}
		return fn()
	})
}

// syncLocked adopts the generation in the epoch file. A change made by another
// process wakes this process's waiters like a local one. The caller holds e.mu.
func (e *Enrollments) syncLocked() error {
	if e.stateDir == "" {
		return nil
	}
	n, err := readEpoch(e.epochPath())
	if err != nil {
		return err
	}
	if n != e.gen {
		e.setGen(n)
	}
	return nil
}

// bumpLocked advances the generation and persists it. The caller holds e.mu and
// the cross-process lock.
func (e *Enrollments) bumpLocked() error {
	if err := e.syncLocked(); err != nil {
		return err
	}
	n := e.gen + 1
	if e.stateDir != "" {
		if err := identity.WritePrivate(e.epochPath(), []byte(strconv.FormatUint(n, 10)+"\n")); err != nil {
			return fmt.Errorf("agent: store the enrollment generation: %w", err)
		}
	}
	e.gen, e.unannounced = n, true
	return nil
}

// announceLocked wakes the waiters of this process for a generation that
// bumpLocked advanced. The caller holds e.mu.
func (e *Enrollments) announceLocked() {
	if e.unannounced {
		e.unannounced = false
		e.wake()
	}
}

// setGen records a new generation and wakes everything that waits. The caller
// holds e.mu.
func (e *Enrollments) setGen(n uint64) {
	e.gen = n
	e.wake()
}

func (e *Enrollments) wake() {
	close(e.moved)
	e.moved = make(chan struct{})
	e.signal()
}

func readEpoch(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("agent: read the enrollment generation: %w", err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("agent: the enrollment generation file %s is corrupt", path)
	}
	return n, nil
}

// Moved reports whether the generation is no longer gen, whoever changed it. A
// running connector calls it regularly, which is how it learns about a re-pairing
// that another process did.
func (e *Enrollments) Moved(gen uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.syncLocked(); err != nil {
		return false
	}
	return e.gen != gen
}

// Snapshot returns the generation, its change channel and the stored enrollment
// as one consistent view: no writer, in this or another process, runs between
// the three reads. A connection attempt starts from it.
func (e *Enrollments) Snapshot(ctx context.Context) (gen uint64, changed <-chan struct{}, enr Enrollment, ok bool, err error) {
	err = e.locked(ctx, func() error {
		if err := e.syncLocked(); err != nil {
			return err
		}
		gen, changed = e.gen, e.moved
		var lerr error
		enr, ok, lerr = e.Load(ctx)
		return lerr
	})
	return gen, changed, enr, ok, err
}
