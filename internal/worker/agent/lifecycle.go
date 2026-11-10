package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// This file is the lifecycle lock of Story 4.10: one exclusive lock on
// <state>/worker.lock that serializes a running Worker with `minion worker
// reset`. Reset runs in another process and cannot make a running Worker drop
// its authenticated Hub connection, so it must never delete the pairing while
// a Worker runs, and a Worker must never start while a reset is deleting. A
// control socket probe cannot give that (Windows has none yet, and a probe
// followed by a prompt is a race); an operating system file lock can: it
// exists on every platform, it is released when the process ends however it
// ends, and it is held from before the Worker opens its state until the state
// is closed.

// LifecycleLockFile is the name of the lifecycle lock file in the state
// directory. It is never deleted: its content is empty and meaningless, only
// the lock on it counts.
const LifecycleLockFile = "worker.lock"

// ModuleLifecycle names the module that holds the lifecycle lock while the
// Worker runs.
const ModuleLifecycle = "lifecycle"

// OrderLifecycle is the start order of the lifecycle module: before the core
// module opens the state, so the state is never open without the lock.
const OrderLifecycle = 50

// ErrStateInUse is returned by [LockLifecycle] when another process holds the
// lifecycle lock of the state directory: a running Worker, a second Worker on
// the same state, or a reset in progress.
var ErrStateInUse = errors.New("agent: the Worker state is in use by another process (a running Worker or a reset in progress)")

// LockLifecycle takes the exclusive lifecycle lock of the state directory
// without waiting and returns the handle that releases it. It creates the
// directory (mode 0700) and the lock file when they do not exist. When another
// process holds the lock it returns an error that wraps [ErrStateInUse]. The
// lock is released by closing the handle or when the process ends.
func LockLifecycle(stateDir string) (io.Closer, error) {
	if stateDir == "" {
		return nil, errors.New("agent: the lifecycle lock needs the state directory")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create the state directory %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, LifecycleLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the lifecycle lock %s: %w", path, err)
	}
	if err := lockFileNB(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errLockHeld) {
			return nil, fmt.Errorf("%w: %s", ErrStateInUse, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &lifecycleLock{f: f}, nil
}

// errLockHeld is what the platform lock code returns when the lock belongs to
// someone else.
var errLockHeld = errors.New("lock held")

type lifecycleLock struct{ f *os.File }

// Close releases the lock. Closing the file releases it on every platform.
func (l *lifecycleLock) Close() error {
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	_ = unlockFile(f)
	return f.Close()
}

func init() {
	if err := RegisterLifecycle(DefaultRegistry); err != nil {
		panic(err)
	}
}

// RegisterLifecycle registers the lifecycle module on r: it holds the
// lifecycle lock of the state directory from before the core module opens the
// state until after it closed it. A Worker that cannot take the lock (because
// another Worker runs on the state, or `minion worker reset` is deleting it)
// fails to start with an error that wraps [ErrStateInUse].
func RegisterLifecycle(r *Registry) error {
	return r.Register(ModuleLifecycle, OrderLifecycle, startLifecycle)
}

func startLifecycle(_ context.Context, env *Env) (StopFunc, error) {
	dir := env.Settings().StateDir
	lock, err := LockLifecycle(dir)
	if err != nil {
		return nil, fmt.Errorf("take the state lock in %s: %w", dir, err)
	}
	return func(context.Context) error { return lock.Close() }, nil
}
