//go:build unix

package adminsock

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// maxSocketPath is a conservative bound under the sockaddr_un path limit
// (104 bytes on macOS and BSD, 108 on Linux).
const maxSocketPath = 100

// ErrPathTooLong says the socket path does not fit a Unix socket address.
var ErrPathTooLong = errors.New("adminsock: the socket path is too long for a Unix socket")

// ErrInUse says another process answers on the socket.
var ErrInUse = errors.New("adminsock: another Hub already answers on the admin socket of this data directory")

// Listen creates the admin socket at path with mode 0600. A leftover socket
// file of a crashed Hub is removed only when nobody answers on it; one that
// answers yields [ErrInUse], and a path that is not a socket is never touched.
func Listen(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("%w (%d bytes, at most %d): use a shorter MINION_DATA_DIR", ErrPathTooLong, len(path), maxSocketPath)
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("adminsock: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("adminsock: restrict %s: %w", path, err)
	}
	return ln, nil
}

func removeStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("adminsock: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("adminsock: %s exists and is not a socket; refusing to remove it", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("adminsock: %s belongs to another user; refusing to remove it", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("%w (%s)", ErrInUse, path)
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, os.ErrNotExist):
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("adminsock: remove the stale socket %s: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("adminsock: cannot tell whether %s is in use: %w", path, err)
	}
}

// Skippable reports whether a Listen error means "no admin socket on this
// Hub" (the path does not fit a Unix socket) rather than a conflict.
func Skippable(err error) bool { return errors.Is(err, ErrPathTooLong) }
