//go:build windows

package agent

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFileNB takes an exclusive LockFileEx lock on the first byte of f without
// waiting. The lock belongs to the handle, so two opens conflict even inside
// one process, and Windows drops it when the process ends.
func lockFileNB(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLockHeld
	}
	return err
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
