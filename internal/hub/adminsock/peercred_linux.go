//go:build linux

package adminsock

import (
	"fmt"
	"net"
	"syscall"
)

// peerUID returns the user id of the process on the other end of c, from the
// kernel (SO_PEERCRED).
func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred *syscall.Ucred
		serr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, fmt.Errorf("adminsock: read the peer credentials: %w", serr)
	}
	return cred.Uid, nil
}
