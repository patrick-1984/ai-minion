//go:build unix && !linux && !darwin && !freebsd

package adminsock

import (
	"errors"
	"net"
)

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("adminsock: peer credentials are not available on this system")
}
