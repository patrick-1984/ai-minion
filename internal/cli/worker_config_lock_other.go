//go:build !unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// configLockWait is how long a config command waits for another one.
const configLockWait = 15 * time.Second

// lockConfigFile takes the config lock by creating path exclusively and
// returns the function that removes it. Without advisory locks, a command that
// is killed while it holds the lock leaves the file behind; the error names
// it, so that the owner can remove it.
func lockConfigFile(path string) (func(), error) {
	deadline := time.Now().Add(configLockWait)
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

// lockConfigFileContext is [lockConfigFile] whose wait also ends with ctx. It
// removes the lock file only after it created it itself.
func lockConfigFileContext(ctx context.Context, path string) (func(), error) {
	deadline := time.Now().Add(configLockWait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for the config lock %s: %w", path, err)
		}
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
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for the config lock %s: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
