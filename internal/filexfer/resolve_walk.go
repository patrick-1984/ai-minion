//go:build linux || darwin

package filexfer

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// resolveWalk opens rel one element at a time from held directory handles:
// every element with O_NOFOLLOW (macOS also O_NOFOLLOW_ANY), every
// intermediate one with O_DIRECTORY and a directory check, every one with a
// mount check against the root. Elements are already checked (no "..", no
// empty element), and each step is relative to a held handle, so the walk
// stays beneath the root by construction.
func (r *Root) resolveWalk(rel string, flags int, perm uint32) (int, error) {
	if rel == "" {
		fd, err := openatRetry(r.fd, ".", flags|unix.O_CLOEXEC|noFollowFlags, perm)
		if err != nil {
			return -1, classifyOpenError(r.fd, ".", rel, err)
		}
		return fd, nil
	}
	elems := strings.Split(rel, "/")
	dirfd, owned := r.fd, false
	release := func() {
		if owned {
			_ = unix.Close(dirfd)
		}
	}
	for _, e := range elems[:len(elems)-1] {
		fd, err := openatRetry(dirfd, e, walkDirFlags, 0)
		if err != nil {
			err = classifyOpenError(dirfd, e, rel, err)
			release()
			return -1, err
		}
		release()
		dirfd, owned = fd, true
		st, err := fstatFd(fd)
		if err != nil {
			release()
			return -1, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			release()
			if st.Mode&unix.S_IFMT == unix.S_IFLNK {
				return -1, symlinkRefusal(nil)
			}
			return -1, &os.PathError{Op: "open", Path: rel, Err: unix.ENOTDIR}
		}
		if !r.sameMount(fd, &st) {
			release()
			return -1, mountRefusal(nil)
		}
	}
	last := elems[len(elems)-1]
	fd, err := openatRetry(dirfd, last, flags|unix.O_CLOEXEC|noFollowFlags, perm)
	if err != nil {
		err = classifyOpenError(dirfd, last, rel, err)
		release()
		return -1, err
	}
	release()
	return fd, nil
}
