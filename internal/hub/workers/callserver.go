package workers

// Story 7.4: the call streams of the Worker that carry the pipe ops (AD-3,
// AD-11, AD-22, AD-23).
//
// A multiplexed session has exactly one reader of its inbound streams: the
// accept loop of the connect handler (streams.go, Story 4.7), which gives each
// stream to the handler of its kind. [CallServer] never accepts streams. It
// registers its ops with [CallServer.Bind] on the connect handler, whose call
// loop (calls.go) reads the op.request of a "ca" stream, answers worker_retire
// itself and hands any other registered op to the call server together with
// the Worker the handshake authenticated:
//
//   - a "ca" stream is one upstream request, for example `mn pipe_send` in a
//     session on another Worker, a Worker-bound API key or MCP on a Worker
//     host;
//   - the Worker's turn channel is an "ev" stream whose first frame is
//     turns.open; the event ingest (events.go) reads the first frame of every
//     "ev" stream and gives a turn channel to [Pipe.ServeTurns]
//     ([Pipe.RegisterTurns]).
//
// # Call stream frames
//
// Like an op stream: one request, then zero or more op.progress, then exactly
// one op.result or error, then the stream closes.
//
//	{"type":"op.request","op":"pipe_send","request_id":"<uuidv7>",
//	 "args":{...},"api_key":"mn_..."}            // api_key is optional
//	{"type":"op.progress","data":{...}}          // the elements of a streaming op
//	{"type":"op.result","result":{...}}
//	{"type":"error","code":"forbidden","message":"..."}
//
// # Who is calling
//
// Without api_key the caller is the Worker identity of the connection (the
// connection is authenticated by the handshake): a principal that acts with
// the permission of a plain owner on the Workers and sessions of its Worker's
// owner, read and message only (authz.KindWorker). With api_key the caller is
// that key, and the key must be bound to exactly the Worker whose connection
// carries the stream (apikey.Service.AuthenticateCall); a key bound to
// another Worker, or to none, is refused with forbidden, and an unknown,
// expired or revoked key with unauthenticated. A key is re-checked every
// [DefaultReauthInterval] while a stream runs (AD-11), and the key is never
// logged, audited or echoed.
//
// The target of a call may be written in the human form
// "worker-name:session-name" (a JSON string); the Hub resolves it in the
// caller's space exactly as REST does.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Timing and limits of the call server.
const (
	// DefaultCallRequestTimeout bounds the wait for the op.request after a
	// call stream was opened.
	DefaultCallRequestTimeout = 10 * time.Second
	// DefaultReauthInterval is how often a running call re-checks its key
	// (AD-11: at least every 30 s).
	DefaultReauthInterval = 30 * time.Second
	// DefaultMaxCalls is the number of call streams one Worker may have open
	// at once.
	DefaultMaxCalls = 16

	callWriteTimeout = 30 * time.Second
)

// FrameStream is the part of a transport stream the servers use.
// *transport.Stream implements it; tests use fakes.
type FrameStream interface {
	ReadFrame() (json.RawMessage, error)
	WriteFrame(v any) error
	Close() error
	Reset()
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// KeyAuthenticator authenticates an API key presented on the call stream of
// a Worker; *apikey.Service implements it.
type KeyAuthenticator interface {
	AuthenticateCall(ctx context.Context, token, workerID string) (apikey.Identity, error)
}

var _ KeyAuthenticator = (*apikey.Service)(nil)

// TargetResolver resolves the human form of a target; *Router implements it.
type TargetResolver interface {
	Resolve(ctx context.Context, p authz.Principal, target string, all bool) (ops.Target, error)
}

var _ TargetResolver = (*Router)(nil)

// CallContext is everything a call handler gets. The arguments are decoded
// and validated; authorisation on the resource is the handler's job (it goes
// through authz.Decide, as for REST).
type CallContext struct {
	// WorkerID is the Worker that opened the stream.
	WorkerID string
	// Principal is the caller.
	Principal authz.Principal
	// Actor is the audit actor: "worker:<id>" or "key:<id>".
	Actor string
	// Op is the catalog declaration of the operation.
	Op ops.Op
	// Args is a pointer to the operation's argument struct.
	Args any
}

// CallFunc executes one operation of a call stream. A streaming operation
// calls emit for each element and returns a nil result; any other returns its
// result. A returned error is answered as an error frame (a *proto.Error keeps
// its code, anything else is "internal").
type CallFunc func(ctx context.Context, c *CallContext, emit func(any) error) (any, error)

// CallServerConfig configures [NewCallServer].
type CallServerConfig struct {
	// Keys authenticates API keys. Nil refuses every key.
	Keys KeyAuthenticator
	// Resolver resolves "worker-name:session-name" targets. Nil accepts only
	// the canonical target object.
	Resolver TargetResolver
	// Owners reads the owner of a Worker; it is needed to resolve human
	// targets for a Worker identity.
	Owners authz.WorkerLookup
	// Catalog declares the ops; the default is ops.Default.
	Catalog *ops.Registry
	// Auditor records write ops and refused credentials. Nil disables it.
	Auditor Auditor
	// RequestTimeout, ReauthInterval and MaxCalls default to the constants
	// of this file.
	RequestTimeout time.Duration
	ReauthInterval time.Duration
	MaxCalls       int
	// Logger receives problems, never arguments or keys. The default is
	// slog.Default().
	Logger *slog.Logger
}

// CallServer serves the call streams of the Workers.
type CallServer struct {
	cfg      CallServerConfig
	log      *slog.Logger
	mu       sync.Mutex
	handlers map[string]CallFunc
	active   map[string]int
	reg      CallOpRegistrar // set by Bind
}

// NewCallServer returns a call server with no ops; register them with
// [CallServer.Handle].
func NewCallServer(cfg CallServerConfig) *CallServer {
	if cfg.Catalog == nil {
		cfg.Catalog = ops.Default
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultCallRequestTimeout
	}
	if cfg.ReauthInterval <= 0 {
		cfg.ReauthInterval = DefaultReauthInterval
	}
	if cfg.MaxCalls <= 0 {
		cfg.MaxCalls = DefaultMaxCalls
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &CallServer{cfg: cfg, log: log, handlers: map[string]CallFunc{}, active: map[string]int{}}
}

// Handle makes op available on call streams. The op must be a Worker-scope
// operation of the catalog, and registered once.
func (s *CallServer) Handle(op string, fn CallFunc) error {
	o, ok := s.cfg.Catalog.Lookup(op)
	if !ok || fn == nil {
		return errors.New("workers: " + op + " is not an operation of the catalog")
	}
	if o.Scope != ops.ScopeWorker {
		return errors.New("workers: " + op + " is not a Worker operation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.handlers[op]; dup {
		return errors.New("workers: " + op + " already has a call handler")
	}
	s.handlers[op] = fn
	if s.reg != nil {
		return s.bind(op)
	}
	return nil
}

// Ops lists the operations that call streams serve.
func (s *CallServer) Ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.handlers))
	for n := range s.handlers {
		out = append(out, n)
	}
	return out
}

func (s *CallServer) slot(workerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[workerID] >= s.cfg.MaxCalls {
		return false
	}
	s.active[workerID]++
	return true
}

func (s *CallServer) release(workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[workerID]--; s.active[workerID] <= 0 {
		delete(s.active, workerID)
	}
}

// Serve answers one call stream of the Worker workerID and closes it. It reads
// the op.request itself; the Hub serves call streams through [CallServer.Bind],
// where the call loop of the connect handler reads the request and hands it
// over, and Serve stays for a caller that owns a stream of its own.
func (s *CallServer) Serve(ctx context.Context, workerID string, st FrameStream) {
	if !s.slot(workerID) {
		s.fail(st, proto.NewError(proto.CodeRateLimited, "too many calls are open on this connection"))
		return
	}
	defer s.release(workerID)

	_ = st.SetReadDeadline(time.Now().Add(s.cfg.RequestTimeout))
	body, err := st.ReadFrame()
	if err != nil {
		st.Reset()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	var req callRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Type != proto.TypeOpRequest {
		s.fail(st, proto.NewError(proto.CodeInvalidArgument, "a call stream starts with an op.request frame"))
		return
	}
	s.serveRequest(ctx, workerID, st, req)
}

func (s *CallServer) reply(st FrameStream, v any) error {
	_ = st.SetWriteDeadline(time.Now().Add(callWriteTimeout))
	return st.WriteFrame(v)
}

func (s *CallServer) fail(st FrameStream, err error) {
	_ = s.reply(st, errorFrameOf(err, s.log))
	_ = st.Close()
}

// CallOpRegistrar is where the call server registers the ops it serves; the
// connect [Handler] and the [Service] implement it ([Handler.HandleCallOp]).
type CallOpRegistrar interface {
	HandleCallOp(op string, fn CallOp) bool
}

var (
	_ CallOpRegistrar = (*Handler)(nil)
	_ CallOpRegistrar = (*Service)(nil)
)

// Bind makes the ops of the server (those registered now and those added
// later with [CallServer.Handle]) ops of the call streams of reg. The call
// streams of a Worker session are read by the one accept loop of the connect
// handler, which reads the op.request and hands the stream over: the call
// server never accepts streams itself. It fails when reg already serves one of
// the ops.
func (s *CallServer) Bind(reg CallOpRegistrar) error {
	s.mu.Lock()
	if s.reg != nil {
		s.mu.Unlock()
		return errors.New("workers: the call server is bound already")
	}
	s.reg = reg
	names := make([]string, 0, len(s.handlers))
	for n := range s.handlers {
		names = append(names, n)
	}
	s.mu.Unlock()
	for _, n := range names {
		if err := s.bind(n); err != nil {
			return err
		}
	}
	return nil
}

func (s *CallServer) bind(op string) error {
	if !s.reg.HandleCallOp(op, func(ctx context.Context, c Call) { s.serveCall(ctx, op, c) }) {
		return errors.New("workers: the Hub already serves " + op + " on call streams")
	}
	return nil
}

// serveCall is the [CallOp] of the op op of the server.
func (s *CallServer) serveCall(ctx context.Context, op string, c Call) {
	st := c.Stream
	if !s.slot(c.Worker.ID) {
		s.fail(st, proto.NewError(proto.CodeRateLimited, "too many calls are open on this connection"))
		return
	}
	defer s.release(c.Worker.ID)
	s.serveRequest(ctx, c.Worker.ID, st, callRequest{Type: proto.TypeOpRequest, Op: op, RequestID: c.RequestID, Args: c.Args, APIKey: c.APIKey})
}

// serveRequest serves a call stream whose op.request was read.
func (s *CallServer) serveRequest(ctx context.Context, workerID string, st FrameStream, req callRequest) {
	reply := func(v any) error { return s.reply(st, v) }
	fail := func(err error) { s.fail(st, err) }
	s.mu.Lock()
	fn := s.handlers[req.Op]
	s.mu.Unlock()
	op, known := s.cfg.Catalog.Lookup(req.Op)
	if fn == nil || !known {
		fail(proto.NewError(proto.CodeUnsupportedOp, "the Hub does not serve this operation on call streams"))
		return
	}

	key := req.APIKey
	pr, actor, err := s.authenticate(ctx, workerID, key)
	if err != nil {
		s.auditCall(ctx, actor, "call.auth", workerID, denialOutcome(err))
		fail(err)
		return
	}
	args, err := s.decodeArgs(ctx, pr, workerID, op, req.Args)
	if err != nil {
		fail(err)
		return
	}
	c := &CallContext{WorkerID: workerID, Principal: pr, Actor: actor, Op: op, Args: args}

	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// The request is complete: any further byte or the end of the stream
	// means the caller is gone.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = st.ReadFrame()
		cancel(errCallerGone)
	}()
	// The call is over when this function returns: wake the reader whether the
	// caller is still there or not, and wait for it, so that nothing of the
	// call outlives it.
	defer func() {
		_ = st.SetReadDeadline(time.Now())
		<-gone
	}()
	if key != "" {
		go s.reauthenticate(cctx, cancel, workerID, key)
	}

	var res any
	if op.Stream {
		res, err = fn(cctx, c, func(v any) error {
			if err := context.Cause(cctx); err != nil {
				return err
			}
			return tooLargeAsError(reply(map[string]any{"type": proto.TypeOpProgress, "data": v}))
		})
		if err == nil {
			res = struct{}{}
		}
	} else {
		res, err = fn(cctx, c, func(any) error { return errors.New("workers: not a streaming operation") })
	}
	cause := context.Cause(cctx)
	var withdrawn *proto.Error
	if err != nil && errors.As(cause, &withdrawn) {
		err = withdrawn // the credential was withdrawn while the call ran
	}
	if op.ChangesState() {
		s.auditCall(ctx, actor, op.Name, callTarget(args), writeAuditOutcome(res, err))
	}
	if errors.Is(cause, errCallerGone) {
		st.Reset()
		return
	}
	if err != nil {
		fail(err)
		return
	}
	if res == nil {
		res = struct{}{}
	}
	if err := reply(map[string]any{"type": proto.TypeOpResult, "result": res}); err != nil {
		if errors.Is(err, proto.ErrFrameTooLarge) {
			fail(tooLargeAsError(err)) // nothing was written: answer with an error frame
			return
		}
		st.Reset()
		return
	}
	_ = st.Close()
}

// tooLargeAsError turns the error of a frame that is larger than the stream
// limit into a frame_too_large error that is answered as an error frame
// (nothing of the oversized frame was written, so the stream is intact and
// the caller learns what happened instead of seeing a reset). Any other
// error, nil included, is returned as it is.
func tooLargeAsError(err error) error {
	if err != nil && errors.Is(err, proto.ErrFrameTooLarge) {
		return proto.NewError(proto.CodeFrameTooLarge, "the answer is larger than one frame; ask for less (for example a smaller limit) or page on")
	}
	return err
}

var errCallerGone = errors.New("workers: the caller closed the call stream")

// errorFrameOf maps an error to the wire error frame. Errors that are not
// *proto.Error are internal and their text is not sent.
func errorFrameOf(err error, log *slog.Logger) proto.ErrorFrame {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Frame()
	}
	if log != nil {
		log.Error("workers: call failed", "error", err.Error())
	}
	return proto.NewError(proto.CodeInternal, "").Frame()
}

// authenticate decides who a call stream speaks for. The returned actor is
// set even on failure, for the audit row.
func (s *CallServer) authenticate(ctx context.Context, workerID, key string) (authz.Principal, string, error) {
	if key == "" {
		return authz.NewWorkerPrincipal(workerID), "worker:" + workerID, nil
	}
	actor := "worker:" + workerID
	if s.cfg.Keys == nil {
		return authz.Principal{}, actor, proto.NewError(proto.CodeUnauthenticated, "API keys are not accepted on call streams")
	}
	id, err := s.cfg.Keys.AuthenticateCall(ctx, key, workerID)
	switch {
	case errors.Is(err, apikey.ErrInvalidKey):
		return authz.Principal{}, actor, proto.NewError(proto.CodeUnauthenticated, "the API key is not valid")
	case errors.Is(err, apikey.ErrWrongWorker):
		return authz.Principal{}, actor, proto.NewError(proto.CodeForbidden, "the API key is not bound to this Worker")
	case err != nil:
		s.log.Error("workers: cannot authenticate a call key", "worker_id", workerID, "error", err.Error())
		return authz.Principal{}, actor, proto.NewError(proto.CodeInternal, "")
	}
	return id.Principal, "key:" + id.KeyID, nil
}

// reauthenticate withdraws a running call when its key stops authenticating
// (revoked, expired, owner deactivated, AD-11). A failing store is not a
// reason to end the call.
func (s *CallServer) reauthenticate(ctx context.Context, cancel context.CancelCauseFunc, workerID, key string) {
	t := time.NewTicker(s.cfg.ReauthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := s.cfg.Keys.AuthenticateCall(ctx, key, workerID)
		switch {
		case errors.Is(err, apikey.ErrInvalidKey):
			cancel(proto.NewError(proto.CodeUnauthenticated, "the API key is no longer valid"))
			return
		case errors.Is(err, apikey.ErrWrongWorker):
			cancel(proto.NewError(proto.CodeForbidden, "the API key is not bound to this Worker"))
			return
		}
	}
}

// decodeArgs resolves a human target, decodes the arguments into the op's
// argument type and validates them.
func (s *CallServer) decodeArgs(ctx context.Context, pr authz.Principal, workerID string, op ops.Op, raw json.RawMessage) (any, error) {
	obj := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, proto.NewError(proto.CodeInvalidArgument, "args must be a JSON object")
		}
	}
	if t, ok := obj["target"]; ok {
		var human string
		if json.Unmarshal(t, &human) == nil {
			if s.cfg.Resolver == nil {
				return nil, proto.NewError(proto.CodeInvalidArgument, "target must be {worker_id, session_name}")
			}
			rp := pr
			if rp.Kind == authz.KindWorker && s.cfg.Owners != nil {
				owner, err := s.cfg.Owners.WorkerOwner(ctx, workerID)
				if err != nil {
					return nil, proto.NewError(proto.CodeForbidden, "not allowed")
				}
				rp.UserID = owner // resolve in the space of the Worker's owner
			}
			tgt, err := s.cfg.Resolver.Resolve(ctx, rp, human, false)
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(tgt)
			obj["target"] = b
		}
	}
	b, _ := json.Marshal(obj)
	args := op.NewArgs()
	if err := json.Unmarshal(b, args); err != nil {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the arguments do not fit the operation")
	}
	if v, ok := args.(ops.Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, proto.NewError(proto.CodeInvalidArgument, err.Error())
		}
	}
	return args, nil
}

func callTarget(args any) string {
	if a, ok := args.(*ops.PipeSendArgs); ok {
		return a.Target.WorkerID + ":" + a.Target.SessionName
	}
	return ""
}

func writeAuditOutcome(res any, err error) string {
	if err != nil {
		return "failed"
	}
	if wr, ok := res.(ops.WriteResult); ok && wr.Outcome != ops.OutcomeDelivered {
		return string(wr.Outcome)
	}
	return "success"
}

// denialOutcome is the audit outcome of a refused credential.
func denialOutcome(err error) string {
	if proto.CodeOf(err) == proto.CodeInternal {
		return "failed"
	}
	return "denied"
}

func (s *CallServer) auditCall(ctx context.Context, actor, action, target, outcome string) {
	if s.cfg.Auditor == nil {
		return
	}
	if err := s.cfg.Auditor.Record(context.WithoutCancel(ctx), audit.Event{Actor: actor, Action: action, Target: target, Outcome: outcome}); err != nil {
		s.log.Error("workers: audit write failed", "action", action, "error", err.Error())
	}
}

// RegisterCalls makes pipe_send, pipe_read and pipe_wait available on call
// streams, served by the Hub pipe p.
func (p *Pipe) RegisterCalls(cs *CallServer) error {
	if err := cs.Handle(ops.OpPipeSend, func(ctx context.Context, c *CallContext, _ func(any) error) (any, error) {
		res, err := p.Send(ctx, c.Principal, *c.Args.(*ops.PipeSendArgs))
		if err != nil {
			return nil, err
		}
		return res, nil
	}); err != nil {
		return err
	}
	if err := cs.Handle(ops.OpPipeRead, func(ctx context.Context, c *CallContext, _ func(any) error) (any, error) {
		view, err := p.Read(ctx, c.Principal, *c.Args.(*ops.PipeReadArgs))
		if err != nil {
			return nil, err
		}
		return view, nil
	}); err != nil {
		return err
	}
	return cs.Handle(ops.OpPipeWait, func(ctx context.Context, c *CallContext, emit func(any) error) (any, error) {
		return nil, p.Wait(ctx, c.Principal, *c.Args.(*ops.PipeWaitArgs), func(t ops.PipeTurn) error { return emit(t) })
	})
}

// TurnSink serves the turn channel of a Worker after its first frame
// (turns.open); *Pipe implements it.
type TurnSink interface {
	ServeTurns(ctx context.Context, workerID string, st FrameStream)
}

var _ TurnSink = (*Pipe)(nil)
