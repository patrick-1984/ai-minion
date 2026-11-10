//go:build windows

package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipeBufferSize is the size of the in and out buffers of the control pipe.
// A frame is at most 1 MiB; the buffers only smooth short writes.
const pipeBufferSize = 64 << 10

// processUserSID returns the SID of the user that runs the process.
func processUserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read the process token: %w", err)
	}
	return u.User.Sid.String(), nil
}

// Listen creates the control pipe at path, which must be a name made by
// [PipeName], and returns its listener. The pipe is created with the DACL of
// [PipeSDDL]: only the user whose SID is in the name, and SYSTEM, can open it,
// and go-winio refuses remote clients (PIPE_REJECT_REMOTE_CLIENTS). The first
// instance is created exclusively, so a second Worker, or any process that
// holds the name, makes Listen fail with [ErrAlreadyRunning] instead of
// replacing the pipe.
func Listen(path string) (net.Listener, error) {
	sid, err := ParsePipeName(path)
	if err != nil {
		return nil, err
	}
	self, err := processUserSID()
	if err != nil {
		return nil, fmt.Errorf("localapi: %w", err)
	}
	if !pipeServedBy(sid, self) {
		return nil, fmt.Errorf("localapi: the control pipe %s belongs to another user than the one that runs the Worker (%s)", path, self)
	}
	sddl, err := PipeSDDL(sid)
	if err != nil {
		return nil, err
	}
	return listenPipe(path, sddl)
}

// listenPipe creates the pipe at path with the given security descriptor. It
// is [Listen] after the checks of the name and of the user; tests use it with
// names of their own, so that they never hold the pipe of the real Worker.
func listenPipe(path, sddl string) (net.Listener, error) {
	l, err := winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    pipeBufferSize,
		OutputBufferSize:   pipeBufferSize,
	})
	if err != nil {
		// A pipe that exists answers an exclusive create with "pipe busy" or,
		// when another user created it, "access denied".
		if errors.Is(err, windows.ERROR_PIPE_BUSY) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf("%w (%s is held by another process: a running Worker, or a process of another user)", ErrAlreadyRunning, path)
		}
		return nil, fmt.Errorf("localapi: listen on %s: %w", path, err)
	}
	return l, nil
}

// VerifySocket checks that path is a Minion control pipe name. Unlike a unix
// socket, a pipe has no file whose owner can be read before connecting, so the
// check of who created it is made on the open connection by [VerifyConn].
// An error wraps [ErrUntrusted].
func VerifySocket(path string) error {
	if _, err := ParsePipeName(path); err != nil {
		return fmt.Errorf("%w: %v", ErrUntrusted, err)
	}
	return nil
}

// Dial connects to the control pipe at path within timeout.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(path, &timeout)
}

// DialContext connects to the control pipe at path and gives up when ctx ends:
// a busy pipe is retried until then, and an expired deadline is reported as
// [context.DeadlineExceeded]. It is the one place that dials a named pipe, so
// go-winio has this package as its only importer (AD-1); the client wraps it.
func DialContext(ctx context.Context, path string) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		if errors.Is(err, winio.ErrTimeout) {
			return nil, fmt.Errorf("%w: %v", context.DeadlineExceeded, err)
		}
		return nil, err
	}
	return conn, nil
}

// pipeOwner is a seam for [VerifyConn]: it returns the SID of the owner of the
// pipe behind conn.
func pipeOwner(conn net.Conn) (string, error) {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return "", errors.New("the connection is not a named pipe")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("read the pipe owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return "", fmt.Errorf("read the pipe owner: %v", err)
	}
	return owner.String(), nil
}

// VerifyConn checks, on a connection the client just opened, that the pipe was
// created by this user, by SYSTEM or by an administrator (see
// [TrustedPipeOwner]). Pipe names are machine-wide, so another user could have
// created the pipe of this user before the Worker did; the client then must not
// send its request. The error wraps [ErrUntrusted].
func VerifyConn(conn net.Conn) error {
	self, err := processUserSID()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUntrusted, err)
	}
	owner, err := pipeOwner(conn)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUntrusted, err)
	}
	if !TrustedPipeOwner(owner, self) {
		return fmt.Errorf("%w: the pipe is owned by %s", ErrUntrusted, owner)
	}
	return nil
}
