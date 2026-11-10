//go:build !windows

package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// euid is the effective user id; tests replace it.
var euid = os.Geteuid

// createPrivateTemp creates a temporary file in dir. os.CreateTemp already
// creates it with mode 0600 (O_EXCL, perm 0600, before the umask), so the file
// is never visible with wider permissions.
func createPrivateTemp(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

// syncDir flushes the directory entry of a renamed file. Best effort.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// CheckPrivate returns an error when path is not a regular file or when it is
// accessible to its group or to others (any of the mode bits 0077).
func CheckPrivate(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return checkMode(path, fi.Mode())
}

// checkMode is the pure part of CheckPrivate.
func checkMode(path string, mode os.FileMode) error {
	if !mode.IsRegular() {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is not a regular file", path))
	}
	if mode.Perm()&0o077 != 0 {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s has mode %04o; group and others must have no access (chmod 600)", path, mode.Perm()))
	}
	return nil
}

// OpenPrivate opens the existing secret file path for reading and checks the
// file it opened: it must be a regular file, owned by the effective user, with
// no access for group or others. The path is opened once, with O_NOFOLLOW (a
// symbolic link is refused even when its target is private) and O_NONBLOCK (a
// FIFO cannot block the caller), and every check is made on that handle
// (fstat), so there is no window between a check by path and the read in which
// the file can be swapped. Nothing is changed on the file: an exposed file stays
// as it is and the caller decides what to tell the operator. A missing file is
// an error that matches os.ErrNotExist. The caller closes the handle.
func OpenPrivate(path string) (*os.File, error) {
	var fd int
	var err error
	for {
		fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, newErr(kindInvalidArgument, fmt.Sprintf("%s is a symbolic link; it must be the file itself", path))
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	if err := CheckPrivateFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// CheckPrivateFile is the check of [OpenPrivate] on an open file: a regular
// file owned by the effective user that group and others cannot access.
func CheckPrivateFile(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := checkMode(f.Name(), fi.Mode()); err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != euid() {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is owned by uid %d, not by the user that runs this program (uid %d)", f.Name(), st.Uid, euid()))
	}
	return nil
}

// CheckPrivateDir returns an error when another local user could replace what
// lies in dir. The path is resolved one component at a time, as the kernel does,
// and every directory and every symbolic link met on the way is checked,
// including the directories that a link points through: an intermediate hop in
// a directory that others can write would let them redirect the path although
// its start and its end are safe. A directory must be owned by root or by the
// effective user and must not be writable by group or others. A group can have
// other members and no list of trusted writers exists, so the primary group of
// the user does not count as trusted. A sticky directory such as /tmp may be
// writable above dir, because it lets a user remove or rename only their own
// entries, but dir itself may not. A symbolic link must belong to root or to the
// effective user, and a chain of more than 40 links (or a loop) is refused. A
// missing directory is an error that matches os.ErrNotExist.
//
// Every later open of a name below dir goes through the same links, and each of
// them is in a directory that only trusted users can change, so there is no
// unchecked hop to swap.
func CheckPrivateDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	cur := string(filepath.Separator)
	if err := checkTrustedDir(cur, true); err != nil {
		return err
	}
	rest := splitPath(abs)
	hops := 0
	for len(rest) > 0 {
		comp := rest[0]
		rest = rest[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur) // cur is a physical path, so this is what the kernel does
			continue
		}
		next := filepath.Join(cur, comp)
		fi, err := os.Lstat(next)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if err := checkTrustedOwner(next, fi); err != nil {
				return err
			}
			if hops++; hops > maxLinkHops {
				return newErr(kindInvalidArgument, fmt.Sprintf("%s is part of a chain of more than %d symbolic links or a loop", next, maxLinkHops))
			}
			target, err := os.Readlink(next)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				cur = string(filepath.Separator)
			}
			rest = append(splitPath(target), rest...)
			continue
		}
		if err := checkTrustedDir(next, true); err != nil {
			return err
		}
		cur = next
	}
	return checkTrustedDirFinal(cur)
}

// maxLinkHops bounds the symbolic links followed while a path is checked.
const maxLinkHops = 40

// splitPath splits p into its components without cleaning it: a ".." that follows
// a link must be resolved by the kernel's rules, not lexically.
func splitPath(p string) []string {
	return strings.Split(p, string(filepath.Separator))
}

// checkTrustedOwner refuses an object that is owned by neither root nor the effective user.
func checkTrustedOwner(p string, fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && int(st.Uid) != euid() {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is owned by uid %d (not by root or by uid %d), so that user can replace what is below it", p, st.Uid, euid()))
	}
	return nil
}

// checkTrustedDir checks a directory met above the final one: trusted owner, not
// a link, and not writable by group or others unless it is sticky (when
// stickyOK).
func checkTrustedDir(p string, stickyOK bool) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is not a directory", p))
	}
	if err := checkTrustedOwner(p, fi); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 != 0 && (!stickyOK || fi.Mode()&os.ModeSticky == 0) {
		return newErr(kindInvalidArgument, fmt.Sprintf("%s is writable by group or others (mode %04o), so another user can replace what is below it", p, fi.Mode().Perm()))
	}
	return nil
}

// checkTrustedDirFinal checks the directory the path resolved to: the same, but
// a sticky world-writable directory is not good enough for the directory itself.
func checkTrustedDirFinal(p string) error { return checkTrustedDir(p, false) }
