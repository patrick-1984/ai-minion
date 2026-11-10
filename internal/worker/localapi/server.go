package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// SocketName is the file name of the control socket in the runtime directory.
const SocketName = "worker.sock"

const (
	// DefaultRequestTimeout is how long a connection has to send its request
	// frame before the server drops it.
	DefaultRequestTimeout = 10 * time.Second
	// DefaultWriteTimeout bounds every frame written to a client, so that a
	// client that stops reading cannot hold an operation for ever.
	DefaultWriteTimeout = 10 * time.Second
	// DefaultMaxConns bounds the connections served at the same time.
	DefaultMaxConns = 64
)

// ErrUnsupported is returned by [Listen] on a system with neither unix
// sockets nor Windows named pipes.
var ErrUnsupported = errors.New("localapi: the local control socket is not supported on this system yet")

// ErrAlreadyRunning is returned by [Listen] when a process answers on the
// socket already: a second Worker must not take it over.
var ErrAlreadyRunning = errors.New("localapi: another process already answers on the control socket")

// ErrUntrusted is returned by [VerifySocket] for a control socket that is not
// private to this user: its directory or the socket belongs to another user,
// or the directory is open to the group or to others.
var ErrUntrusted = errors.New("localapi: the control socket is not private to this user")

// SocketPath returns the control socket path a Worker listens on,
// <runtime dir>/worker.sock. The runtime directory is $XDG_RUNTIME_DIR/minion
// on Linux when that variable is an absolute path; otherwise (and on macOS) it
// is <temp dir>/minion-<uid> (config.PathEnv.RuntimeDir).
//
// On Windows the "socket" is the named pipe \\.\pipe\minion-worker-<user
// SID> (see [PipeName]).
//
// A Worker started without XDG_RUNTIME_DIR (a system service such as
// minion-worker@.service, which has no login session) therefore listens at a
// different path than a login shell of the same user would compute. Clients
// must use [SocketCandidates] to find it.
func SocketPath(env config.PathEnv) (string, error) {
	if env.GOOS == "windows" {
		return windowsPipePath(env)
	}
	dir, err := env.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, SocketName), nil
}

// SocketCandidates returns, in lookup order, the paths where the Worker of
// this user may listen: first [SocketPath] for env, then the path the Worker
// computes when XDG_RUNTIME_DIR is not set (<temp dir>/minion-<uid>/worker.sock).
// A Worker started by a system service has no XDG_RUNTIME_DIR, while the shell
// of the same user usually has. Duplicates are dropped, so the list has one
// element where the variable plays no role (macOS, Windows, or unset).
func SocketCandidates(env config.PathEnv) ([]string, error) {
	first, err := SocketPath(env)
	if err != nil {
		return nil, err
	}
	out := []string{first}
	noXDG := env
	getenv := env.Getenv
	noXDG.Getenv = func(k string) string {
		if k == "XDG_RUNTIME_DIR" {
			return ""
		}
		if getenv == nil {
			return ""
		}
		return getenv(k)
	}
	if second, err := SocketPath(noXDG); err == nil && second != first {
		out = append(out, second)
	}
	return out, nil
}

// RequestFrame is the `op.request` frame. The Hub adds more members (the
// caller's actions and the request id travel in the same frame there); the
// local socket ignores everything but the op and its arguments, because the
// local owner holds every action and a write carries its request id inside
// the arguments (AD-15).
type RequestFrame struct {
	Type string          `json:"type"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// ProgressFrame is the `op.progress` frame: one element of a streaming op.
type ProgressFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ResultFrame is the `op.result` frame: the JSON result of the op.
type ResultFrame struct {
	Type   string          `json:"type"`
	Result json.RawMessage `json:"result"`
}

// FullActions returns every action: the local OS user is the local owner and
// holds all of them (AD-22).
func FullActions() []ops.Action {
	return []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}
}

// Request is one op request as the server hands it to the [Executor].
type Request struct {
	Op string
	// Actions is always [FullActions] on the local socket.
	Actions []ops.Action
	Args    json.RawMessage
}

// Executor runs one op. progress sends one `op.progress` element to the
// caller; it may be called any number of times before Execute returns, from
// any goroutine, and is a no-op once the connection is gone. ctx ends when the
// server stops or the client disconnects.
//
// An error that is (or wraps) a *proto.Error, or that has a
// `ProtoError() *proto.Error` method, is sent to the client as that `error`
// frame. Any other error is logged and sent as a plain `internal` error, so
// that no internal detail leaves the Worker.
type Executor interface {
	Execute(ctx context.Context, rq Request, progress func(json.RawMessage)) (json.RawMessage, error)
}

// ExecutorFunc adapts a function to [Executor].
type ExecutorFunc func(ctx context.Context, rq Request, progress func(json.RawMessage)) (json.RawMessage, error)

// Execute calls f.
func (f ExecutorFunc) Execute(ctx context.Context, rq Request, progress func(json.RawMessage)) (json.RawMessage, error) {
	return f(ctx, rq, progress)
}

// Config assembles a [Server]. Executor is required.
type Config struct {
	Executor Executor
	// Logger receives operational messages: the op name, the outcome class
	// and the duration, never arguments or results (AD-20). Nil discards them.
	Logger *slog.Logger
	// RequestTimeout, WriteTimeout and MaxConns default to the constants of
	// the same name above when zero.
	RequestTimeout time.Duration
	WriteTimeout   time.Duration
	MaxConns       int
}

// Server answers op requests on a listener.
type Server struct {
	exec    Executor
	log     *slog.Logger
	reqTime time.Duration
	wrTime  time.Duration
	sem     chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	listener net.Listener
	closed   bool
	wg       sync.WaitGroup
}

// New returns a server for cfg.
func New(cfg Config) (*Server, error) {
	if cfg.Executor == nil {
		return nil, errors.New("localapi: the server needs an executor")
	}
	s := &Server{exec: cfg.Executor, log: cfg.Logger, reqTime: cfg.RequestTimeout, wrTime: cfg.WriteTimeout}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.reqTime <= 0 {
		s.reqTime = DefaultRequestTimeout
	}
	if s.wrTime <= 0 {
		s.wrTime = DefaultWriteTimeout
	}
	n := cfg.MaxConns
	if n <= 0 {
		n = DefaultMaxConns
	}
	s.sem = make(chan struct{}, n)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// Serve accepts connections on l until [Server.Close] is called, and returns
// nil then. It closes l on return. Serve runs once per server.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed || s.listener != nil {
		s.mu.Unlock()
		_ = l.Close()
		if s.closed {
			return nil
		}
		return errors.New("localapi: the server is already serving")
	}
	s.listener = l
	s.mu.Unlock()
	defer func() { _ = l.Close() }()

	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("localapi: accept: %w", err)
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.serveConn(conn)
		}()
	}
}

// Close stops accepting, cancels the operations in flight (their context
// ends) and waits for the connections to finish or for ctx to end. It is
// idempotent.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	l := s.listener
	s.mu.Unlock()
	s.cancel()
	if l != nil {
		_ = l.Close()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("localapi: connections did not finish: %w", ctx.Err())
	}
}

// serveConn handles one connection: one request, its answer, then close.
func (s *Server) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.writeError(conn, proto.NewError(proto.CodeRateLimited, "too many local connections; retry shortly"))
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(s.reqTime))
	// A shutdown must not wait for a connection that has not sent its request
	// yet: Close ends s.ctx, which ends this read at once.
	stopIdle := context.AfterFunc(s.ctx, func() { _ = conn.SetReadDeadline(time.Unix(1, 0)) })
	body, err := proto.ReadFrame(conn, proto.StreamFrameLimit)
	stopIdle()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return // a probe that connected and left (stale socket check)
		}
		var pe *proto.Error
		if errors.As(err, &pe) {
			s.writeError(conn, pe)
		}
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	rq, perr := parseRequest(body)
	if perr != nil {
		s.writeError(conn, perr)
		return
	}

	// The operation ends when the client goes away: a client sends nothing
	// after its request, so any read result means disconnect or misuse.
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		var b [1]byte
		_, _ = conn.Read(b[:])
		cancel()
	}()
	defer func() {
		_ = conn.Close() // unblocks the watcher
		<-watcherDone
	}()

	w := &frameWriter{conn: conn, timeout: s.wrTime, cancel: cancel}
	start := time.Now()
	// A progress element that cannot be framed (too large, not valid JSON) is
	// dropped, but the operation must not then end as a success: the first
	// such failure stops the operation and decides the final frame.
	var (
		progMu  sync.Mutex
		progErr *proto.Error
	)
	progressFailure := func() *proto.Error {
		progMu.Lock()
		defer progMu.Unlock()
		return progErr
	}
	raw, err := s.execute(ctx, rq, func(data json.RawMessage) {
		werr := w.writeErr(ProgressFrame{Type: proto.TypeOpProgress, Data: nullIfEmpty(data)})
		var pe *proto.Error
		if werr == nil || !errors.As(werr, &pe) {
			return // sent, or the connection is gone (the writer cancelled the op)
		}
		progMu.Lock()
		if progErr == nil {
			progErr = pe
		}
		progMu.Unlock()
		cancel()
	})
	if pf := progressFailure(); pf != nil {
		s.log.Error("local op progress could not be sent", "op", rq.Op, "code", string(pf.Code), "error", pf.Message)
		w.write(proto.NewError(proto.CodeInternal, "the operation produced a progress element that cannot be sent").Frame())
		return
	}
	if err != nil {
		if ctx.Err() != nil {
			s.log.Debug("local op cancelled", "op", rq.Op, "duration", time.Since(start).String())
			return
		}
		pe := s.mapError(rq.Op, err)
		s.log.Debug("local op failed", "op", rq.Op, "code", string(pe.Code), "duration", time.Since(start).String())
		w.write(pe.Frame())
		return
	}
	res := ResultFrame{Type: proto.TypeOpResult, Result: nullIfEmpty(raw)}
	if err := w.writeErr(res); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) { // too large or not encodable: tell the client why
			w.write(pe.Frame())
		}
		return
	}
	s.log.Debug("local op done", "op", rq.Op, "duration", time.Since(start).String())
}

// execute calls the executor and turns a panic into an error.
func (s *Server) execute(ctx context.Context, rq Request, progress func(json.RawMessage)) (raw json.RawMessage, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in op %s: %v", rq.Op, p)
		}
	}()
	return s.exec.Execute(ctx, rq, progress)
}

func parseRequest(body []byte) (Request, *proto.Error) {
	typ, err := proto.FrameType(body)
	if err != nil {
		return Request{}, asProto(err)
	}
	if typ != proto.TypeOpRequest {
		return Request{}, proto.NewError(proto.CodeInvalidArgument, "expected an op.request frame, got "+typ)
	}
	var f RequestFrame
	if err := proto.DecodeFrame(body, &f); err != nil {
		return Request{}, asProto(err)
	}
	if f.Op == "" {
		return Request{}, proto.NewError(proto.CodeInvalidArgument, "op.request needs an op name")
	}
	return Request{Op: f.Op, Actions: FullActions(), Args: f.Args}, nil
}

// protoErrorer is implemented by errors that know their wire form.
type protoErrorer interface{ ProtoError() *proto.Error }

func (s *Server) mapError(op string, err error) *proto.Error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	var pr protoErrorer
	if errors.As(err, &pr) {
		if pe := pr.ProtoError(); pe != nil {
			return pe
		}
	}
	s.log.Error("local op failed", "op", op, "error", err)
	return proto.NewError(proto.CodeInternal, "the operation failed on the Worker")
}

func asProto(err error) *proto.Error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	return proto.NewError(proto.CodeInvalidArgument, err.Error())
}

func (s *Server) writeError(conn net.Conn, pe *proto.Error) {
	w := &frameWriter{conn: conn, timeout: s.wrTime, cancel: func() {}}
	w.write(pe.Frame())
}

func nullIfEmpty(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

// frameWriter serialises the frames of one connection. A failed write cancels
// the operation: the client is gone or stalled.
type frameWriter struct {
	mu      sync.Mutex
	conn    net.Conn
	timeout time.Duration
	cancel  context.CancelFunc
	dead    bool
}

func (w *frameWriter) write(v any) { _ = w.writeErr(v) }

func (w *frameWriter) writeErr(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return net.ErrClosed
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
	err := proto.WriteFrame(w.conn, proto.StreamFrameLimit, v)
	if err == nil {
		return nil
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return err // nothing was written; the connection is still usable
	}
	w.dead = true
	w.cancel()
	return err
}

// Capability-qualified operations.
//
// The local socket carries one request per connection, and a Worker daemon that
// is older than the client ignores every argument it does not know (AD-4). A
// client therefore cannot rely on the daemon to refuse a guarded request it does
// not understand, and a preflight on another connection proves nothing about the
// daemon that gets the request (it may be replaced in between). The request
// itself carries the requirement: a client that needs features of the daemon
// names the operation "<op>+<feature>[+<feature>...]", for example
// "answer+answer.reviewed". A daemon that has the features strips the suffix and
// runs the operation; a daemon that does not know the qualified name, which is
// every older one, refuses it as an unsupported operation before anything
// happens, on the very connection the request was sent on. Nothing is split into
// two requests and nothing is negotiated by a claim of the client: the daemon
// checks the requirement against what it implements.

// QualifiedOp returns the wire name of op for a request that needs the given
// features, or op itself when it needs none.
func QualifiedOp(op string, need []string) string {
	if len(need) == 0 {
		return op
	}
	return op + "+" + strings.Join(need, "+")
}

// SplitOp is the inverse of [QualifiedOp]: the operation and the features the
// request requires.
func SplitOp(wire string) (op string, need []string) {
	op, rest, ok := strings.Cut(wire, "+")
	if !ok {
		return wire, nil
	}
	return op, strings.Split(rest, "+")
}
