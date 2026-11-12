//go:build !unix

package adminsock

import (
	"errors"
	"net"
)

// ErrPathTooLong is never returned on this system.
var ErrPathTooLong = errors.New("adminsock: the socket path is too long for a Unix socket")

// ErrInUse is never returned on this system.
var ErrInUse = errors.New("adminsock: another Hub already answers on the admin socket of this data directory")

// ErrUnsupported says the system has no Unix sockets with peer credentials.
var ErrUnsupported = errors.New("adminsock: the local admin socket is not available on this system")

// Listen is not available on this system.
func Listen(string) (net.Listener, error) { return nil, ErrUnsupported }

func peerUID(*net.UnixConn) (uint32, error) { return 0, ErrUnsupported }

// Skippable reports whether a Listen error means "no admin socket on this
// Hub" rather than a conflict.
func Skippable(err error) bool { return errors.Is(err, ErrUnsupported) }
