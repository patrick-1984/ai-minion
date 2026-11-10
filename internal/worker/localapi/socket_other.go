//go:build !unix && !windows

package localapi

import "net"

// Listen is not available on a system with neither unix sockets nor Windows
// named pipes; the Worker runs without a local socket there.
func Listen(path string) (net.Listener, error) { return nil, ErrUnsupported }

// VerifySocket accepts every path: without unix sockets there is no file to
// check.
func VerifySocket(path string) error { return nil }
