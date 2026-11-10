//go:build !windows

package version

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxLinkHops bounds the symbolic links followed while a path is checked.
const maxLinkHops = 40

// checkTrustedDir returns an error when a user other than root or the effective
// user could replace, add or remove entries in dir or on the way to it. The path
// is resolved one component at a time, as the kernel does, and every directory
// and every symbolic link met on the way is checked, including the directories
// that a link points through: an intermediate hop in a directory that others
// can write would let them redirect the path although its start and its end are
// safe. A directory must be owned by root or by the effective user and must not
// be writable by group or others (a group can have other members, so the primary
// group does not count as trusted). A sticky directory such as /tmp may be
// writable above dir, because it lets a user remove or rename only their own
// entries, but dir itself may not. A symbolic link must belong to root or to the
// effective user, and a chain of more than 40 links (or a loop) is refused. A
// missing directory is an error that matches os.ErrNotExist.
//
// Every later open of a name below dir goes through the same links, each in a
// directory that only trusted users can change, so there is no unchecked hop to
// swap. (The same algorithm as identity.CheckPrivateDir.)
func checkTrustedDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	euid := os.Geteuid()
	cur := string(filepath.Separator)
	if err := checkTrustedDirAt(cur, true, euid); err != nil {
		return err
	}
	rest := strings.Split(abs, string(filepath.Separator))
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
			if err := checkTrustedOwner(next, fi, euid); err != nil {
				return err
			}
			if hops++; hops > maxLinkHops {
				return errUntrusted(fmt.Sprintf("%s is part of a chain of more than %d symbolic links or a loop", next, maxLinkHops))
			}
			target, err := os.Readlink(next)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				cur = string(filepath.Separator)
			}
			rest = append(strings.Split(target, string(filepath.Separator)), rest...)
			continue
		}
		if err := checkTrustedDirAt(next, true, euid); err != nil {
			return err
		}
		cur = next
	}
	return checkTrustedDirAt(cur, false, euid)
}

// checkTrustedOwner refuses an object that is owned by neither root nor the
// effective user.
func checkTrustedOwner(p string, fi os.FileInfo, euid int) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && int(st.Uid) != euid {
		return errUntrusted(fmt.Sprintf("%s is owned by uid %d (not by root or by uid %d), so that user can replace what is below it", p, st.Uid, euid))
	}
	return nil
}

// checkTrustedDirAt checks one directory: trusted owner, not a link, and not
// writable by group or others unless it is sticky (when stickyOK).
func checkTrustedDirAt(p string, stickyOK bool, euid int) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errUntrusted(fmt.Sprintf("%s is not a directory", p))
	}
	if err := checkTrustedOwner(p, fi, euid); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 != 0 && (!stickyOK || fi.Mode()&os.ModeSticky == 0) {
		return errUntrusted(fmt.Sprintf("%s is writable by group or others (mode %04o), so another user can replace what is below it", p, fi.Mode().Perm()))
	}
	return nil
}

// openTrusted opens the existing regular file path for reading and checks the
// file it opened: it is not a symbolic link (the path is opened once with
// O_NOFOLLOW and O_NONBLOCK), it is owned by root or by the effective user and
// group and others cannot write it. Every check is made on the handle, so the
// bytes read are the bytes that were checked. The file may be readable by
// others (it is not secret). The caller closes the handle.
func openTrusted(path string) (*os.File, error) {
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
			return nil, errUntrusted(fmt.Sprintf("%s is a symbolic link; it must be the file itself", path))
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err == nil {
		err = checkTrustedFileInfo(path, fi)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkTrustedFileInfo(path string, fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return errUntrusted(fmt.Sprintf("%s is not a regular file", path))
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return errUntrusted(fmt.Sprintf("%s has mode %04o; group and others must not be able to write it", path, fi.Mode().Perm()))
	}
	return checkTrustedOwner(path, fi, os.Geteuid())
}

// checkInstallDir has nothing to add on Unix: replacing an executable needs
// write permission on its directory, which only the owner, root or a group
// that the administrator chose (for example staff on /usr/local/bin) has, and
// refusing such a directory would break ordinary installations. The Windows
// counterpart checks the ACLs, whose inheritance can hand write access to
// objects that are created below a directory.
func checkInstallDir(string) error { return nil }

// checkNewFile checks a file this program has just created: trusted owner, and
// no write access for group or others. It runs on the open handle.
func checkNewFile(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return checkTrustedFileInfo(f.Name(), fi)
}
