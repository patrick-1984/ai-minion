//go:build windows

package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// dialLocal connects to the Worker's named pipe at path (AD-22). Pipe names
// are machine-wide, so after the connect it checks who created the pipe
// ([localapi.VerifyConn]) and closes the connection without sending anything
// when that is another user: the client never sends a request to a pipe that
// a foreign user planted.
func dialLocal(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := localapi.DialContext(dctx, path)
	if err != nil {
		return nil, mapDialError(ctx, path, err)
	}
	if err := localapi.VerifyConn(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w (pipe %s): %v", ErrPermissionDenied, path, err)
	}
	return conn, nil
}

// mapDialError maps a failed connect to the errors of this package: a pipe
// that does not exist means no Worker runs, a pipe that refuses this user is
// a permission problem, and a busy pipe that stayed busy until the timeout
// means the Worker does not answer.
func mapDialError(ctx context.Context, path string, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return fmt.Errorf("%w (pipe %s)", ErrWorkerNotRunning, path)
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return fmt.Errorf("%w (pipe %s)", ErrPermissionDenied, path)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("the Minion Worker did not answer on the control pipe %s in time", path)
	}
	return fmt.Errorf("connect to the Minion control pipe %s: %w", path, err)
}
