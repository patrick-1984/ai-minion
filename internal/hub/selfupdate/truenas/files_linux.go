//go:build linux

package truenas

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens a file for reading without following a final symlink and
// without blocking on a FIFO or device (O_NONBLOCK); callers check the type
// with Stat on the handle.
func openNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func checkOwner(fi os.FileInfo, uid int) error {
	if uid < 0 {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		return fmt.Errorf("must be owned by uid %d", uid)
	}
	return nil
}

// lockFile takes an exclusive, non-blocking flock on f.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrBusy
	}
	return err
}
