//go:build !windows

package localapi

import (
	"context"
	"errors"
	"net"
	"time"
)

// processUserSID is the SID of the process user, which exists on Windows only.
func processUserSID() (string, error) {
	return "", errors.New("localapi: user SIDs exist on Windows only")
}

// Dial connects to the control socket at path within timeout. It is what the
// callers use to find out whether a Worker answers; they must not rely on the
// connection carrying any trust (the client checks the socket first, see
// [VerifySocket]).
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

// DialContext connects to the control socket at path and gives up when ctx
// ends. The same trust rule as for [Dial] applies.
func DialContext(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
