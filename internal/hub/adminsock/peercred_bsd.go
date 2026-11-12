//go:build darwin || freebsd

package adminsock

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the user id of the process on the other end of c, from the
// kernel (LOCAL_PEERCRED).
func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred *unix.Xucred
		serr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, fmt.Errorf("adminsock: read the peer credentials: %w", serr)
	}
	return cred.Uid, nil
}
