//go:build !windows

package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"
	"time"
)

// dialLocal connects to the unix socket at path.
func dialLocal(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", path)
	switch {
	case err == nil:
		return conn, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return nil, fmt.Errorf("%w (socket %s)", ErrWorkerNotRunning, path)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("%w (socket %s)", ErrPermissionDenied, path)
	}
	return nil, fmt.Errorf("connect to the Minion control socket %s: %w", path, err)
}
