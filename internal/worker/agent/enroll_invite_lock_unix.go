//go:build unix

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockConfig takes the exclusive advisory lock on the config lock file at path,
// the same lock `minion worker config` takes (flock), and returns the function
// that releases it. The kernel drops it when the process dies.
func lockConfig(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the config lock %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// lockEnrollment takes the exclusive advisory lock of the enrollment on the
// file at path and returns the function that releases it. It waits until the
// lock is free or ctx ends, so that SIGINT or SIGTERM interrupts a start that
// waits for another process's enrollment. The kernel drops the lock when the
// process dies.
func lockEnrollment(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the enrollment lock %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(enrollLockPoll):
		}
	}
}
