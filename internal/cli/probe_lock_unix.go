//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockPollInterval is how often a waiting probe retries the advisory lock.
const lockPollInterval = 10 * time.Millisecond

// lockFile takes an exclusive advisory lock on the file at path and returns
// the function that releases it. The directory is created with mode 0700 and
// must belong to the current user; the file is created with mode 0600 and is
// never reached through a symbolic link. The wait ends with ctx. The kernel
// drops the lock when the process dies, so a crashed probe cannot leave it
// behind.
func lockFile(ctx context.Context, path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the lock directory %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("check the lock directory %s: %w", dir, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("the lock directory %s is not a directory of this user", dir)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the lock %s: %w", path, err)
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
			return nil, fmt.Errorf("wait for another mn command of this session: %w", ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}
