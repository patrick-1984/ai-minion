//go:build unix

package cli

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// serviceOwnedPrivate reads path with the daemon's private-file rules for
// the account uid that will run the service: not a link, a regular file
// owned by uid, no access for group or others.
func serviceOwnedPrivate(path string, uid int) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file (a link is refused)", os.ErrInvalid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: readable by others", os.ErrPermission)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return nil, fmt.Errorf("%w: not owned by the service account", os.ErrPermission)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSinkFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSinkFile {
		return nil, fmt.Errorf("the file is larger than %d bytes", maxSinkFile)
	}
	return b, nil
}
