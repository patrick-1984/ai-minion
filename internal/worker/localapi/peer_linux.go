//go:build linux

package localapi

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID returns the pid of the process at the other end of a Unix socket
// connection (SO_PEERCRED), or 0.
func peerPID(conn net.Conn) int {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil && cred.Pid > 0 {
			pid = int(cred.Pid)
		}
	})
	return pid
}
