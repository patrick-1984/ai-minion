package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// DefaultDialTimeout bounds the connect to the local socket.
const DefaultDialTimeout = 5 * time.Second

// LocalBackend is the [Backend] over the Worker's local control socket. It
// holds no connection: every call opens one, sends one `op.request` frame and
// reads `op.progress` frames until the `op.result` or `error` frame
// (AD-3, AD-22). It is safe for concurrent use.
type LocalBackend struct {
	socket      string
	dialTimeout time.Duration
}

var _ Backend = (*LocalBackend)(nil)

// LocalOption configures [Local].
type LocalOption func(*LocalBackend)

// WithSocketPath uses the socket at path instead of looking for the default
// ones (see [Local]). The same safety checks apply to it.
func WithSocketPath(path string) LocalOption {
	return func(b *LocalBackend) { b.socket = path }
}

// WithDialTimeout changes the bound of the connect.
func WithDialTimeout(d time.Duration) LocalOption {
	return func(b *LocalBackend) { b.dialTimeout = d }
}

// Local returns the backend for the local control socket. On Windows the
// socket is the named pipe \\.\pipe\minion-worker-<user SID> (AD-22): the
// candidate is that one name, and before the client sends a request it checks
// that the pipe was created by this user, SYSTEM or an administrator.
//
// Without [WithSocketPath], every call looks for the Worker's socket in the
// order of localapi.SocketCandidates: first <runtime dir>/minion/worker.sock
// from XDG_RUNTIME_DIR (Linux), then <temp dir>/minion-<uid>/worker.sock, where
// a Worker without XDG_RUNTIME_DIR (a system service) listens. The first
// candidate that exists and answers is used. When none does, the error is
// [ErrWorkerNotRunning] and names every path tried.
//
// The client only talks to a socket it can trust: before it dials, it checks
// that the socket and its directory belong to the current user and that the
// directory is closed to others (localapi.VerifySocket). Anything else is
// refused with [ErrPermissionDenied] and never connected to, so another user
// cannot plant a socket in a shared temp directory to read the requests of
// this user or to forge results.
func Local(opts ...LocalOption) *LocalBackend {
	b := &LocalBackend{dialTimeout: DefaultDialTimeout}
	for _, o := range opts {
		o(b)
	}
	return b
}

// candidates returns the socket paths to try, in order.
func (b *LocalBackend) candidates() ([]string, error) {
	if b.socket != "" {
		return []string{b.socket}, nil
	}
	c, err := localapi.SocketCandidates(config.SystemPathEnv())
	if err != nil {
		return nil, fmt.Errorf("find the Minion control socket: %w", err)
	}
	return c, nil
}

// connect opens a connection to the first trusted socket that answers.
func (b *LocalBackend) connect(ctx context.Context) (net.Conn, error) {
	paths, err := b.candidates()
	if err != nil {
		return nil, err
	}
	var denied error // the first refusal, reported when nothing else answers
	for _, p := range paths {
		if err := localapi.VerifySocket(p); err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case errors.Is(err, localapi.ErrUntrusted):
				if denied == nil {
					denied = fmt.Errorf("%w (socket %s): %v", ErrPermissionDenied, p, err)
				}
			default:
				return nil, fmt.Errorf("check the Minion control socket %s: %w", p, err)
			}
			continue
		}
		conn, err := b.dial(ctx, p)
		switch {
		case err == nil:
			return conn, nil
		case errors.Is(err, ErrPermissionDenied):
			if denied == nil {
				denied = err
			}
		case errors.Is(err, ErrWorkerNotRunning):
			// Nobody answers on this one (a stale file): try the next.
		default:
			return nil, err
		}
	}
	if denied != nil {
		return nil, denied
	}
	return nil, fmt.Errorf("%w (socket: %s)", ErrWorkerNotRunning, strings.Join(paths, ", "))
}

// Call implements [Backend].
func (b *LocalBackend) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	return b.Stream(ctx, op, args, nil)
}

// Stream implements [Backend].
func (b *LocalBackend) Stream(ctx context.Context, op string, args any, onProgress func(json.RawMessage) error) (json.RawMessage, error) {
	if op == "" {
		return nil, errors.New("client: an op name is required")
	}
	raw, err := encodeArgs(args)
	if err != nil {
		return nil, err
	}
	// A request that relies on a feature of the Worker daemon is sent under the
	// capability-qualified name of the op (see localapi.QualifiedOp). The daemon
	// checks it on this very connection before anything happens; a daemon that
	// predates the feature does not know the name and refuses it, instead of
	// ignoring the arguments it does not understand and reporting "delivered".
	need := ops.LocalRequiredFeatures(op, raw)
	wire := localapi.QualifiedOp(op, need)
	conn, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	// Closing the connection is how a cancelled context interrupts a read.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	res, err := exchange(conn, wire, raw, onProgress)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var pe *proto.Error
	if err != nil && len(need) > 0 && errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp {
		return nil, fmt.Errorf("%w: the running Worker is older than this client and does not support %s safely (it needs %s); update and restart the Worker. Nothing was sent: %w",
			ErrWorkerTooOld, op, strings.Join(need, ", "), pe)
	}
	return res, err
}

// dial opens a connection to the control socket (a unix socket, or the named
// pipe on Windows) and maps the failures to the errors of this package.
func (b *LocalBackend) dial(ctx context.Context, path string) (net.Conn, error) {
	return dialLocal(ctx, path, b.dialTimeout)
}

// exchange runs one request on conn.
func exchange(conn net.Conn, op string, args json.RawMessage, onProgress func(json.RawMessage) error) (json.RawMessage, error) {
	req := localapi.RequestFrame{Type: proto.TypeOpRequest, Op: op, Args: args}
	if err := proto.WriteFrame(conn, proto.StreamFrameLimit, req); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return nil, pe // too large: nothing was sent
		}
		return nil, fmt.Errorf("%w: %v", ErrConnectionLost, err)
	}
	for {
		body, err := proto.ReadFrame(conn, proto.StreamFrameLimit)
		if err != nil {
			var pe *proto.Error
			if errors.As(err, &pe) {
				return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
			}
			return nil, fmt.Errorf("%w: %v", ErrConnectionLost, err)
		}
		typ, err := proto.FrameType(body)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
		}
		switch typ {
		case proto.TypeOpProgress:
			var f localapi.ProgressFrame
			if err := proto.DecodeFrame(body, &f); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
			}
			if onProgress != nil {
				if err := onProgress(f.Data); err != nil {
					return nil, err
				}
			}
		case proto.TypeOpResult:
			var f localapi.ResultFrame
			if err := proto.DecodeFrame(body, &f); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
			}
			return f.Result, nil
		case proto.TypeError:
			var f proto.ErrorFrame
			if err := proto.DecodeFrame(body, &f); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
			}
			return nil, f.Err()
		default:
			return nil, fmt.Errorf("%w: type %q", ErrProtocol, typ)
		}
	}
}

// encodeArgs encodes args as the JSON "args" member; nil means no arguments.
func encodeArgs(args any) (json.RawMessage, error) {
	switch a := args.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return a, nil
	case []byte:
		return json.RawMessage(a), nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the arguments are not encodable as JSON: "+err.Error())
	}
	return b, nil
}
