//go:build unix

package localapi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// DirMode and SocketMode are the protection of the control socket: only
	// the owning OS user can reach it (AD-22, R22).
	DirMode    os.FileMode = 0o700
	SocketMode os.FileMode = 0o600

	// maxSocketPath is a conservative bound under the sockaddr_un path limit
	// (104 bytes on macOS and BSD, 108 on Linux).
	maxSocketPath = 100

	probeTimeout = time.Second

	// lockSuffix names the lock file next to the socket.
	lockSuffix = ".lock"
)

// Listen creates the control socket at path and returns its listener. The
// directory is created with mode 0700 when missing and must belong to the
// current user (a looser mode of our own directory is tightened). A socket
// file that is left over from a crashed Worker is removed only when no
// process answers on it; a live one yields [ErrAlreadyRunning], and a path
// that is not a socket is never touched. The socket gets mode 0600. Concurrent
// calls for the same path are serialised by an advisory lock on
// <path>.lock, so exactly one of them wins a stale or missing socket.
func Listen(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("localapi: the socket path %q must be absolute", path)
	}
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("localapi: the socket path %q is longer than %d bytes, which unix sockets do not allow; set a shorter XDG_RUNTIME_DIR", path, maxSocketPath)
	}
	if err := prepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	// Two Workers that start at the same time must not both judge the socket
	// stale and then replace each other's live socket: probe, removal and bind
	// run under an inter-process lock.
	unlock, err := lockDir(path + lockSuffix)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := removeStale(path); err != nil {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("localapi: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, SocketMode); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("localapi: restrict %s: %w", path, err)
	}
	return l, nil
}

// prepareDir makes sure dir is a real directory of this user with mode 0700.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("localapi: create %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("localapi: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("localapi: %s is not a directory", dir)
	}
	if !ownedByMe(fi) {
		return fmt.Errorf("localapi: %s belongs to another user; refusing to put the control socket there", dir)
	}
	if fi.Mode().Perm() != DirMode {
		if err := os.Chmod(dir, DirMode); err != nil {
			return fmt.Errorf("localapi: restrict %s to its owner: %w", dir, err)
		}
	}
	return nil
}

// removeStale deletes a leftover socket file when nobody answers on it.
func removeStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("localapi: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("localapi: %s exists and is not a socket; refusing to remove it", path)
	}
	if !ownedByMe(fi) {
		return fmt.Errorf("localapi: %s belongs to another user; refusing to remove it", path)
	}
	conn, err := net.DialTimeout("unix", path, probeTimeout)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("%w (%s)", ErrAlreadyRunning, path)
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, os.ErrNotExist):
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("localapi: remove the stale socket %s: %w", path, err)
		}
		return nil
	default:
		// Timeout, permission denied: we cannot tell that nobody answers.
		return fmt.Errorf("localapi: cannot tell whether %s is in use: %w", path, err)
	}
}

func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// lockDir takes an exclusive advisory lock on the file at path (created with
// mode 0600) and returns the function that releases it. The lock is held only
// for the short probe-and-bind sequence of [Listen], and the kernel drops it
// when the process dies, so a crashed Worker cannot leave it behind.
func lockDir(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, SocketMode)
	if err != nil {
		return nil, fmt.Errorf("localapi: open the start lock %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("localapi: lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil // closing the file releases the lock
}

// VerifySocket checks that the control socket at path is private to this user,
// without following symbolic links and without connecting: the parent
// directory is a real directory owned by the current user with no group or
// other permission bits, and path itself is a socket owned by the current
// user. A client calls it before it dials, so that it never sends requests to
// a socket that another user planted, for example in a shared temp directory.
//
// A missing directory or socket yields an error that wraps [fs.ErrNotExist];
// anything else that fails the check wraps [ErrUntrusted].
func VerifySocket(path string) error { return verifySocket(path, ownedByMe) }

func verifySocket(path string, owned func(os.FileInfo) bool) error {
	dir := filepath.Dir(path)
	di, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("localapi: %w", err)
	}
	switch {
	case !di.IsDir():
		return fmt.Errorf("%w: %s is not a real directory", ErrUntrusted, dir)
	case !owned(di):
		return fmt.Errorf("%w: %s belongs to another user", ErrUntrusted, dir)
	case di.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%w: %s is accessible to other users (mode %v)", ErrUntrusted, dir, di.Mode().Perm())
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("localapi: %w", err)
	}
	switch {
	case fi.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%w: %s is not a socket", ErrUntrusted, path)
	case !owned(fi):
		return fmt.Errorf("%w: %s belongs to another user", ErrUntrusted, path)
	}
	return nil
}
