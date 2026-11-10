//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package agent

import (
	"errors"
	"os"
	"syscall"
)

// lockFileNB takes an exclusive flock(2) on f without waiting. flock locks
// belong to the open file description, so two opens conflict even inside one
// process, and the kernel drops the lock when the process dies.
func lockFileNB(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errLockHeld
		}
		return err
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
