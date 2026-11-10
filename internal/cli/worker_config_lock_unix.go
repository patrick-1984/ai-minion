//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// configLockPoll is how often a waiting command retries the config lock.
const configLockPoll = 10 * time.Millisecond

// lockConfigFile takes an exclusive advisory lock on the file at path (created
// with mode 0600) and returns the function that releases it. The kernel drops
// the lock when the process dies, so a crashed command cannot leave it behind.
func lockConfigFile(path string) (func(), error) {
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
	return func() { _ = f.Close() }, nil // closing the file releases the lock
}

// lockConfigFileContext is [lockConfigFile] whose wait ends with ctx: the
// lock is tried without blocking and retried, so that SIGINT or SIGTERM
// interrupts a command that waits for another one. It never releases a lock
// it does not hold; when ctx ends it only closes its own descriptor.
func lockConfigFileContext(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wait for the config lock %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the config lock %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil // closing the file releases the lock
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("wait for the config lock %s: %w", path, ctx.Err())
		case <-time.After(configLockPoll):
		}
	}
}
