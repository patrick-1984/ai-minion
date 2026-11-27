//go:build !linux

package localapi

import "net"

// peerPID cannot tell the peer process on this platform.
func peerPID(net.Conn) int { return 0 }
