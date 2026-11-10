//go:build !unix

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// lockConfig takes the config lock by creating path exclusively, like
// `minion worker config` does without advisory locks, and returns the function
// that removes it.
func lockConfig(path string) (func(), error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("open the config lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another config command holds %s; if none runs, remove that file", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// enrollLockStaleAfter is how long a platform without advisory locks waits for
// the lock file before it assumes that a crash left it behind. It is longer
// than an enrollment run with the default retries.
const enrollLockStaleAfter = 5 * time.Minute

// lockEnrollment takes the lock of the enrollment by creating path exclusively
// and returns the function that removes it. It waits until the lock is free or
// ctx ends. Without advisory locks a crash leaves the file behind: the error
// after the wait names it.
func lockEnrollment(ctx context.Context, path string) (func(), error) {
	deadline := time.Now().Add(enrollLockStaleAfter)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("open the enrollment lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another enrollment holds %s; if none runs, remove that file", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(enrollLockPoll):
		}
	}
}
