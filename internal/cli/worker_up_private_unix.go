//go:build unix

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// serviceOwnedPrivate reads path with the daemon's private-file rules for
// the account (uid, gid) that will run the service: every directory on the
// way must be searchable by that account (root could read files the service
// cannot), and the opened handle (no link followed, no blocking) must be a
// regular file owned by uid with no access for group or others. The checks
// are made on the handle that is read, so a file swapped after a check is
// not validated in its place. Supplementary groups are not known here: a
// directory reachable only through one is refused.
func serviceOwnedPrivate(path string, uid, gid int) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		fi, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		if !searchable(fi, uid, gid) {
			return nil, fmt.Errorf("%w: a directory on the way cannot be searched by the service account", os.ErrPermission)
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", os.ErrInvalid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: readable by others", os.ErrPermission)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return nil, fmt.Errorf("%w: not owned by the service account", os.ErrPermission)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSinkFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSinkFile {
		return nil, fmt.Errorf("the file is larger than %d bytes", maxSinkFile)
	}
	return b, nil
}

// searchable reports whether the account (uid, gid) may search the
// directory fi by its owner, group or other bits.
func searchable(fi os.FileInfo, uid, gid int) bool {
	if uid == 0 {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	m := fi.Mode().Perm()
	switch {
	case int(st.Uid) == uid:
		return m&0o100 != 0
	case int(st.Gid) == gid:
		return m&0o010 != 0
	}
	return m&0o001 != 0
}
