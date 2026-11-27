//go:build linux

package filexfer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// openat2Supported reports whether this build has an openat2 resolver.
const openat2Supported = true

// resolveBeneath is the resolution of every open beneath a root (BRD 5.3).
const resolveBeneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_MAGICLINKS

// openat2Retry calls openat2, retrying EINTR and the EAGAIN that the kernel
// returns when a concurrent rename or mount raced a ".." or a mount check.
func openat2Retry(dirfd int, path string, how *unix.OpenHow) (int, error) {
	var err error
	for range 16 {
		var fd int
		fd, err = unix.Openat2(dirfd, path, how)
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.EINTR) && !errors.Is(err, unix.EAGAIN) {
			return -1, err
		}
	}
	return -1, err
}

// resolveOpenat2 opens rel beneath the root with one openat2 call: "..",
// absolute paths, every symlink, magic links and every mount crossing fail in
// the kernel, atomically.
func (r *Root) resolveOpenat2(rel string, flags int, perm uint32) (int, error) {
	name := rel
	if name == "" {
		name = "."
	}
	how := unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Resolve: resolveBeneath} //nolint:gosec // open flags are not negative
	if flags&(unix.O_CREAT|unix.O_TMPFILE) != 0 {
		how.Mode = uint64(perm)
	}
	fd, err := openat2Retry(r.fd, name, &how)
	if err != nil {
		switch {
		case errors.Is(err, unix.ELOOP):
			return -1, symlinkRefusal(err)
		case errors.Is(err, unix.EXDEV):
			// RESOLVE_BENEATH and RESOLVE_NO_XDEV both answer EXDEV; ".."
			// and absolute paths are refused before, so it is a mount.
			return -1, mountRefusal(err)
		case errors.Is(err, unix.ENOSYS):
			return -1, rootUnknown("openat2 is not available", err)
		case errors.Is(err, unix.ENOTDIR):
			return -1, classifyWalkNotDir(r, rel, err)
		}
		return -1, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	return fd, nil
}

// classifyWalkNotDir tells a symlink in the way (which openat2 can report as
// ENOTDIR when it is the last element and O_DIRECTORY is set) from a file.
func classifyWalkNotDir(r *Root, rel string, err error) error {
	var st unix.Stat_t
	how := unix.OpenHow{Flags: unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: resolveBeneath}
	fd, oerr := openat2Retry(r.fd, rel, &how)
	if oerr == nil {
		defer func() { _ = unix.Close(fd) }()
		if unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return symlinkRefusal(err)
		}
	}
	return &os.PathError{Op: "open", Path: rel, Err: err}
}

// openAbsOpenat2 opens the clean absolute directory path with
// RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS: a symlink anywhere in it fails.
func openAbsOpenat2(path string) (int, error) {
	if err := checkAbs(path); err != nil {
		return -1, err
	}
	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := openat2Retry(unix.AT_FDCWD, path, &how)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return -1, rootUnknown("the path of the folder of the session contains a symbolic link", err)
		}
		return -1, rootUnknown("the folder of the session cannot be opened", err)
	}
	return fd, nil
}
