//go:build !linux

package localapi

import "net"

// peerPID is 0 where the Worker does not read the peer credentials.
func peerPID(net.Conn) int { return 0 }
