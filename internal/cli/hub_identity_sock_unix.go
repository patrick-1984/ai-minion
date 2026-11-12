//go:build unix

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// checkSocketFile makes sure the admin socket is the Hub's own before a
// passphrase is sent to it: a socket, owned by the Hub account (the owner of
// the data directory that holds it), and neither group- nor other-writable.
// The caller must be that account or root (root may run the command on behalf
// of the Hub account).
func checkSocketFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		if fi.Mode()&os.ModeSocket == 0 {
			return errors.New("not a socket")
		}
		return nil
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	dst, ok := di.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return validateSocketOwner(fi.Mode(), int(st.Uid), int(dst.Uid), os.Geteuid())
}

// validateSocketOwner is the rule of checkSocketFile on plain values: the
// socket's owner must be the expected Hub uid (the owner of the data
// directory), the socket must not be writable by group or others, and the
// caller (euid) must be that uid or root.
func validateSocketOwner(mode fs.FileMode, sockUID, hubUID, euid int) error {
	if mode&fs.ModeSocket == 0 {
		return errors.New("not a socket")
	}
	if sockUID != hubUID {
		return fmt.Errorf("the socket is not owned by the Hub account (uid %d, expected %d): %w", sockUID, hubUID, os.ErrPermission)
	}
	if mode.Perm()&0o022 != 0 {
		return fmt.Errorf("the socket is writable by group or others: %w", os.ErrPermission)
	}
	if euid != 0 && euid != hubUID {
		return fmt.Errorf("the socket belongs to another user: %w", os.ErrPermission)
	}
	return nil
}
