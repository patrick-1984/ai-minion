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
//	 "args":{...},"api_key":"mn_...",            // api_key is optional
//	 "source_session":"boss"}                    // source_session is optional
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
// With source_session, when an admin gave that session of the Worker a
// session-control grant (store table session_control), the caller is the
// Worker identity carrying the grant (authz.NewWorkerGrantPrincipal), even
// when the frame also carries api_key: the grant wins over a call key. The
// grant is read on every call and re-read every [DefaultReauthInterval] while
// a call runs; a removed or changed grant ends it. Without a grant row
// nothing changes. The ops registered with [CallServer.HandleGrantOnly]
// (session control, session_control_calls.go) are served only to a caller
// with a grant; anyone else gets forbidden.
//
// The target of a call may be written in the human form
// "worker-name:session-name" (a JSON string); the Hub resolves it in the
// caller's space exactly as REST does.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
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

// PrincipalChecker reports whether a principal still has any permission.
type PrincipalChecker interface {
	Valid(ctx context.Context, p authz.Principal) (bool, error)
}

var _ PrincipalChecker = (*authz.Authorizer)(nil)

// GrantStore reads the session-control grant of a controller session; it
// returns store.ErrNotFound when the session has none. *store.Store
// implements it.
type GrantStore interface {
	SessionControl(ctx context.Context, workerID, sessionName string) (store.SessionControlRow, error)
	// SessionGrantsFor returns the direct session grants of the session
	// (identified by name), expired and suspended ones included.
	SessionGrantsFor(ctx context.Context, workerID, sessionName string) ([]store.SessionGrantRow, error)
}

var _ GrantStore = (*store.Store)(nil)

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
	// SourceSession is the validated source_session of the request, or "".
	SourceSession string
	// RequestWorkerID is the worker_id member of the arguments object, read
	// before decoding (a UUIDv7, or ""): it names the Worker of an op whose
	// arguments name none (sessions_list), as the worker_id of a REST request.
	RequestWorkerID string
	// admit is the dispatch fence of a call that a grant authorized: the
	// Router runs it on the opened connection, before it writes the request.
	admit func(context.Context, Online) error
	// grantIDs are the direct grants that authorized the routed request, as
	// the Router read them at its decision; for the audit event of a write.
	grantIDs []string
}

// CallFunc executes one operation of a call stream. A streaming operation
// calls emit for each element and returns a nil result; any other returns its
// result. A returned error is answered as an error frame (a *proto.Error keeps
// its code, anything else is "internal").
type CallFunc func(ctx context.Context, c *CallContext, emit func(any) error) (any, error)

// CallServerConfig configures [NewCallServer].
type CallServerConfig struct {
	// Telemetry returns the traffic telemetry (call counts); nil: none.
	Telemetry func() Telemetry
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
	// Authz re-checks the Worker identity of a keyless call while it runs:
	// the call ends when the Worker's owner lost access (F-07), as a call
	// with an API key ends when its key stops authenticating. Nil skips the
	// check. *authz.Authorizer implements it.
	Authz PrincipalChecker
	// Auditor records write ops and refused credentials. Nil disables it.
	Auditor Auditor
	// Grants reads session-control grants. Nil means no session has one.
	Grants GrantStore
	// Now is the clock that judges the expiry of direct grants at the
	// dispatch fence and at the re-check of a running call. The default is
	// time.Now.
	Now func() time.Time
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
	cfg       CallServerConfig
	log       *slog.Logger
	telemetry func() Telemetry
	mu        sync.Mutex
	handlers  map[string]CallFunc
	// grantOnly are the ops served only to a caller with a grant.
	grantOnly map[string]bool
	active    map[string]int
	reg       CallOpRegistrar // set by Bind
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &CallServer{cfg: cfg, log: log, telemetry: cfg.Telemetry, handlers: map[string]CallFunc{}, grantOnly: map[string]bool{}, active: map[string]int{}}
}

// Handle makes op available on call streams. The op must be a Worker-scope
// operation of the catalog, and registered once.
func (s *CallServer) Handle(op string, fn CallFunc) error {
	return s.handle(op, fn, false)
}

// HandleGrantOnly is [CallServer.Handle] for an op that is served only to a
// caller with a session-control grant; any other caller gets forbidden before
// its arguments are read. Besides the Worker-scope ops it accepts workers_list,
// the one Hub op a grant reaches.
func (s *CallServer) HandleGrantOnly(op string, fn CallFunc) error {
	return s.handle(op, fn, true)
}

func (s *CallServer) handle(op string, fn CallFunc, grantOnly bool) error {
	o, ok := s.cfg.Catalog.Lookup(op)
	if !ok || fn == nil {
		return errors.New("workers: " + op + " is not an operation of the catalog")
	}
	if o.Scope != ops.ScopeWorker && (!grantOnly || op != ops.OpWorkersList) {
		return errors.New("workers: " + op + " is not a Worker operation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.handlers[op]; dup {
		return errors.New("workers: " + op + " already has a call handler")
	}
	s.handlers[op] = fn
	s.grantOnly[op] = grantOnly
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
	s.serveRequest(ctx, c.Worker.ID, st, callRequest{Type: proto.TypeOpRequest, Op: op, RequestID: c.RequestID, Args: c.Args, APIKey: c.APIKey, SourceSession: c.SourceSession})
}

// serveRequest serves a call stream whose op.request was read.
func (s *CallServer) serveRequest(ctx context.Context, workerID string, st FrameStream, req callRequest) {
	reply := func(v any) error { return s.reply(st, v) }
	// The decision of the call, written at debug level when it ends: which
	// credential spoke for it and how it ended.
	start, credential, outcome := time.Now(), "", "ok"
	defer func() {
		if s.telemetry != nil {
			if t := s.telemetry(); t != nil {
				t.CallServed(workerID, req.Op, outcome)
			}
		}
		s.log.Debug("workers: call stream", "op", req.Op, "worker_id", workerID, "source_session", req.SourceSession,
			"credential", credential, "outcome", outcome, "duration", time.Since(start).String())
	}()
	fail := func(err error) {
		outcome = string(proto.CodeInternal)
		var pe *proto.Error
		if errors.As(err, &pe) {
			outcome = string(pe.Code)
		}
		s.fail(st, err)
	}
	s.mu.Lock()
	fn, grantOnly := s.handlers[req.Op], s.grantOnly[req.Op]
	s.mu.Unlock()
	op, known := s.cfg.Catalog.Lookup(req.Op)
	if fn == nil || !known {
		fail(proto.NewError(proto.CodeUnsupportedOp, "the Hub does not serve this operation on call streams"))
		return
	}
	if req.SourceSession != "" && (ops.Target{SessionName: req.SourceSession}).Validate() != nil {
		fail(proto.NewError(proto.CodeInvalidArgument, "source_session is not a valid session name"))
		return
	}

	key := req.APIKey
	pr, actor, grant, err := s.authenticate(ctx, workerID, key, req.SourceSession, req.Op)
	direct := directGrantOps[req.Op]
	switch {
	case grant != "":
		credential = "session_control_grant"
	case key != "":
		credential = "api_key"
	default:
		credential = "worker"
	}
	if err != nil {
		s.auditCall(ctx, actor, "call.auth", workerID, denialOutcome(err))
		fail(err)
		return
	}
	if grantOnly && grant == "" {
		fail(proto.NewError(proto.CodeForbidden, "not allowed"))
		return
	}
	args, reqWorker, err := s.decodeArgs(ctx, pr, workerID, op, req.Args)
	if err != nil {
		fail(err)
		return
	}
	c := &CallContext{WorkerID: workerID, Principal: pr, Actor: actor, Op: op, Args: args,
		SourceSession: req.SourceSession, RequestWorkerID: reqWorker}

	if grant != "" {
		c.admit = s.admitGrant(workerID, req.SourceSession, grant, direct)
	}

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
	switch {
	case grant != "":
		go s.reauthorizeWorker(cctx, cancel, pr, req.SourceSession, grant, direct)
	case key != "":
		go s.reauthenticate(cctx, cancel, workerID, key)
	case s.cfg.Authz != nil:
		go s.reauthorizeWorker(cctx, cancel, pr, "", "", false)
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
		ev := audit.Event{Actor: actor, Action: op.Name, Target: callTarget(args), Outcome: writeAuditOutcome(res, err)}
		if req.SourceSession != "" {
			ev.Source = workerID + ":" + req.SourceSession
		}
		// The direct grants that authorized the write (empty when the
		// session-control grant or the own-Worker rule did, and when the
		// write was refused before the Router decided it).
		ev.GrantID = strings.Join(c.grantIDs, ",")
		s.auditEvent(ctx, ev)
	}
	if errors.Is(cause, errCallerGone) {
		outcome = "caller_gone"
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
		// The caller never got its answer: not a success.
		outcome = "reply_failed"
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

// directGrantOps are the operations a direct session grant reaches on call
// streams (D3), whatever its level: the level decides, through the actions it
// gives, which of them a holder is served. For every other op the call stream
// serves (pipe_send, pipe_read, pipe_wait and any op added later) the direct
// grants are not read at all: the caller is who it was without them.
var directGrantOps = map[string]bool{
	ops.OpWorkersList: true, ops.OpSessionsList: true, ops.OpSessionSnapshot: true, ops.OpSessionCapture: true,
	ops.OpSendText: true, ops.OpSendKeys: true, ops.OpSetTask: true, ops.OpAnswer: true,
}

// authenticate decides who a call stream speaks for on the operation op. A
// session-control grant of the source session wins over the key; grant is
// then the stored targets of the grant (for the re-check), else "". Direct
// session grants count only for the operations of directGrantOps. The
// returned actor is set even on failure, for the audit row.
func (s *CallServer) authenticate(ctx context.Context, workerID, key, source, op string) (pr authz.Principal, actor, grant string, err error) {
	actor = "worker:" + workerID
	if grant, err = s.lookupGrant(ctx, workerID, source); err != nil {
		s.log.Error("workers: cannot read a session-control grant", "worker_id", workerID, "error", err.Error())
		return authz.Principal{}, actor, "", proto.NewError(proto.CodeInternal, "")
	}
	var direct []store.SessionGrantRow
	if directGrantOps[op] {
		if direct, err = s.lookupDirect(ctx, workerID, source); err != nil {
			s.log.Error("workers: cannot read the session grants", "worker_id", workerID, "error", err.Error())
			return authz.Principal{}, actor, "", proto.NewError(proto.CodeInternal, "")
		}
	}
	if len(direct) > 0 {
		// Direct session grants: the session principal carries them beside
		// the session-control grant (nil scope when it has none).
		var legacy authz.Scope
		if grant != "" {
			var targets []ops.KeyResource
			if err := json.Unmarshal([]byte(grant), &targets); err != nil {
				s.log.Error("workers: a session-control grant cannot be read", "worker_id", workerID, "error", err.Error())
				return authz.Principal{}, actor, "", proto.NewError(proto.CodeInternal, "")
			}
			legacy = apikey.NewScope(targets, authz.GrantActions())
		}
		return authz.NewWorkerSessionPrincipal(workerID, source, legacy, directGrantsOf(direct)), actor,
			grant + directDigest(direct, s.cfg.Now()), nil
	}
	if grant != "" {
		var targets []ops.KeyResource
		if err := json.Unmarshal([]byte(grant), &targets); err != nil {
			s.log.Error("workers: a session-control grant cannot be read", "worker_id", workerID, "error", err.Error())
			return authz.Principal{}, actor, "", proto.NewError(proto.CodeInternal, "")
		}
		return authz.NewWorkerGrantPrincipal(workerID, apikey.NewScope(targets, authz.GrantActions())), actor, grant, nil
	}
	if key == "" {
		return authz.NewWorkerPrincipal(workerID), actor, "", nil
	}
	if s.cfg.Keys == nil {
		return authz.Principal{}, actor, "", proto.NewError(proto.CodeUnauthenticated, "API keys are not accepted on call streams")
	}
	id, err := s.cfg.Keys.AuthenticateCall(ctx, key, workerID)
	switch {
	case errors.Is(err, apikey.ErrInvalidKey):
		return authz.Principal{}, actor, "", proto.NewError(proto.CodeUnauthenticated, "the API key is not valid")
	case errors.Is(err, apikey.ErrWrongWorker):
		return authz.Principal{}, actor, "", proto.NewError(proto.CodeForbidden, "the API key is not bound to this Worker")
	case err != nil:
		s.log.Error("workers: cannot authenticate a call key", "worker_id", workerID, "error", err.Error())
		return authz.Principal{}, actor, "", proto.NewError(proto.CodeInternal, "")
	}
	pr = id.Principal
	if pr.KeyID == "" {
		pr = pr.WithKeyID(id.KeyID)
	}
	return pr, "key:" + id.KeyID, "", nil
}

// lookupGrant returns the stored targets of the session-control grant of the
// session source on the Worker, or "" when it has none.
func (s *CallServer) lookupGrant(ctx context.Context, workerID, source string) (string, error) {
	if source == "" || s.cfg.Grants == nil {
		return "", nil
	}
	row, err := s.cfg.Grants.SessionControl(ctx, workerID, source)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	return row.TargetsJSON, nil
}

// lookupDirect returns the direct session grants of the session source on the
// Worker (by name), nil when it has none.
func (s *CallServer) lookupDirect(ctx context.Context, workerID, source string) ([]store.SessionGrantRow, error) {
	if source == "" || s.cfg.Grants == nil {
		return nil, nil
	}
	return s.cfg.Grants.SessionGrantsFor(ctx, workerID, source)
}

// grantKey is the value the re-check of a running call compares: the stored
// targets of the session-control grant, followed by a digest of the direct
// grants when the session has any and direct is set (the op is one a direct
// grant reaches, as in authenticate). Without direct grants it is exactly the
// stored targets. The digest is taken at the clock of the server, so the key
// of a grant changes at the instant it expires.
func (s *CallServer) grantKey(ctx context.Context, workerID, source string, direct bool) (string, error) {
	grant, err := s.lookupGrant(ctx, workerID, source)
	if err != nil || !direct {
		return grant, err
	}
	rows, err := s.lookupDirect(ctx, workerID, source)
	if err != nil {
		return "", err
	}
	return grant + directDigest(rows, s.cfg.Now()), nil
}

// directDigest identifies the state of direct grant rows at the time now: it
// changes when a row is added, removed, written (revision), suspended or given
// another expiry, and when the clock passes the expiry of a row (a grant is
// valid while now is before expires_at, as authz judges it). It is "" for no
// rows and never valid JSON otherwise.
func directDigest(rows []store.SessionGrantRow, now time.Time) string {
	if len(rows) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rows))
	for _, g := range rows {
		// "none" keeps a grant without expiry apart from every stored
		// expiry, the zero value included.
		exp := "none"
		if g.ExpiresAt != nil {
			exp = fmt.Sprintf("%d/%t", *g.ExpiresAt, now.UnixMilli() >= *g.ExpiresAt)
		}
		parts = append(parts, fmt.Sprintf("%s/%d/%t/%s", g.ID, g.Revision, g.SuspendedAt != nil, exp))
	}
	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "#direct:" + hex.EncodeToString(sum[:])
}

// directGrantsOf maps stored session grants to what the decision judges.
func directGrantsOf(rows []store.SessionGrantRow) []authz.DirectGrant {
	out := make([]authz.DirectGrant, 0, len(rows))
	for _, g := range rows {
		d := authz.DirectGrant{
			ID: g.ID, TargetWorkerID: g.TargetWorkerID, TargetSession: g.TargetSession, Level: g.Level,
			Authority: g.Authority, GrantedBy: g.GrantedBy, SourceOwnerID: g.SourceOwnerID, TargetOwnerID: g.TargetOwnerID,
			Suspended: g.SuspendedAt != nil,
		}
		if g.ExpiresAt != nil {
			// A stored expiry always ends the grant, the zero time included.
			d.ExpiresAt = authz.ExpiryAt(time.UnixMilli(*g.ExpiresAt))
		}
		out = append(out, d)
	}
	return out
}

// admitGrant is the dispatch fence of a call authorized by the grants of the
// session source: it reads them again, as authenticate did, and refuses when
// they are not the ones (key grant) the call was authorized with, or when one
// of them expired since (the key is taken at the clock of the server). A
// failing store refuses too. direct is as for grantKey.
func (s *CallServer) admitGrant(workerID, source, grant string, direct bool) func(context.Context, Online) error {
	return func(ctx context.Context, _ Online) error {
		cur, err := s.grantKey(ctx, workerID, source, direct)
		if err != nil {
			s.log.Error("workers: cannot re-read the grants before a dispatch", "worker_id", workerID, "error", err.Error())
			return proto.NewError(proto.CodeInternal, "")
		}
		if cur != grant {
			return proto.NewError(proto.CodeForbidden, "the grant was removed, changed or expired")
		}
		return nil
	}
}

// reauthenticate withdraws a running call when its key stops authenticating
// (revoked, expired, owner deactivated, AD-11). One failing re-check (a store
// error) is passed over; the second in a row ends the call, as in
// reauthorizeWorker.
func (s *CallServer) reauthenticate(ctx context.Context, cancel context.CancelCauseFunc, workerID, key string) {
	t := time.NewTicker(s.cfg.ReauthInterval)
	defer t.Stop()
	failures := 0
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
		case err == nil:
			failures = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if failures++; failures >= 2 {
			s.log.Error("workers: the key of a running call could not be re-checked twice in a row and the call ends", "worker_id", workerID)
			cancel(proto.NewError(proto.CodeForbidden, "the permission of the call could not be re-checked"))
			return
		}
	}
}

// reauthorizeWorker withdraws a running keyless call when its Worker identity
// stops being valid: the Worker was removed, or its owner was deactivated or
// holds no grant any more (F-07). A call authorized by the session-control
// grant of the session source (stored targets grant) also ends when that
// grant was removed or changed, or when a direct grant of the session expired
// (direct is as for grantKey). One failing re-check (a store error) is
// passed over; the second in a row ends the call.
func (s *CallServer) reauthorizeWorker(ctx context.Context, cancel context.CancelCauseFunc, pr authz.Principal, source, grant string, direct bool) {
	t := time.NewTicker(s.cfg.ReauthInterval)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		failed := false
		if s.cfg.Authz != nil {
			ok, err := s.cfg.Authz.Valid(ctx, pr)
			if err == nil && !ok {
				cancel(proto.NewError(proto.CodeForbidden, "not allowed"))
				return
			}
			failed = err != nil
		}
		if grant != "" {
			cur, err := s.grantKey(ctx, pr.WorkerID, source, direct)
			if err == nil && cur != grant {
				cancel(proto.NewError(proto.CodeForbidden, "the session-control grant was removed, changed or expired"))
				return
			}
			failed = failed || err != nil
		}
		if ctx.Err() != nil {
			return
		}
		if !failed {
			failures = 0
			continue
		}
		if failures++; failures >= 2 {
			s.log.Error("workers: a running call could not be re-checked twice in a row and ends", "worker_id", pr.WorkerID)
			cancel(proto.NewError(proto.CodeForbidden, "the permission of the call could not be re-checked"))
			return
		}
	}
}

// decodeArgs resolves a human target, decodes the arguments into the op's
// argument type and validates them. For sessions_list, whose arguments name no
// Worker, it also returns the worker_id member of the arguments object
// (validated as a UUIDv7 when present), as REST reads it from the request.
func (s *CallServer) decodeArgs(ctx context.Context, pr authz.Principal, workerID string, op ops.Op, raw json.RawMessage) (any, string, error) {
	obj := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, "", proto.NewError(proto.CodeInvalidArgument, "args must be a JSON object")
		}
	}
	var reqWorker string
	if w, ok := obj["worker_id"]; ok && op.Name == ops.OpSessionsList {
		if json.Unmarshal(w, &reqWorker) != nil || !ops.ValidUUIDv7(reqWorker) {
			return nil, "", proto.NewError(proto.CodeInvalidArgument, "worker_id must be a UUIDv7")
		}
	}
	if t, ok := obj["target"]; ok {
		var human string
		if json.Unmarshal(t, &human) == nil {
			if s.cfg.Resolver == nil {
				return nil, "", proto.NewError(proto.CodeInvalidArgument, "target must be {worker_id, session_name}")
			}
			rp := pr
			if rp.Kind == authz.KindWorker && s.cfg.Owners != nil {
				owner, err := s.cfg.Owners.WorkerOwner(ctx, workerID)
				if err != nil {
					return nil, "", proto.NewError(proto.CodeForbidden, "not allowed")
				}
				rp.UserID = owner // resolve in the space of the Worker's owner
			}
			// A grant reaches beyond the owner's own Workers (an admin-owned
			// controller with targets of other owners): resolve among every
			// Worker the grant may read.
			tgt, err := s.cfg.Resolver.Resolve(ctx, rp, human, (pr.Scope != nil || len(pr.DirectGrants()) > 0) && pr.Kind == authz.KindWorker)
			if err != nil {
				return nil, "", err
			}
			b, _ := json.Marshal(tgt)
			obj["target"] = b
		}
	}
	b, _ := json.Marshal(obj)
	args := op.NewArgs()
	if err := json.Unmarshal(b, args); err != nil {
		return nil, "", proto.NewError(proto.CodeInvalidArgument, "the arguments do not fit the operation")
	}
	if v, ok := args.(ops.Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, "", proto.NewError(proto.CodeInvalidArgument, err.Error())
		}
	}
	return args, reqWorker, nil
}

// callTarget is the audit target of a call: "worker_id:session" of the op's
// target, or "".
func callTarget(args any) string {
	if t, ok := targetOfArgs(args); ok {
		return t.WorkerID + ":" + t.SessionName
	}
	return ""
}

// targetOfArgs returns the target of the arguments of a call op that names
// one session.
func targetOfArgs(args any) (ops.Target, bool) {
	switch a := args.(type) {
	case *ops.PipeSendArgs:
		return a.Target, true
	case *ops.SessionSnapshotArgs:
		return a.Target, true
	case *ops.SessionCaptureArgs:
		return a.Target, true
	case *ops.SendTextArgs:
		return a.Target, true
	case *ops.SendKeysArgs:
		return a.Target, true
	case *ops.SetTaskArgs:
		return a.Target, true
	case *ops.AnswerArgs:
		return a.Target, true
	}
	return ops.Target{}, false
}

func writeAuditOutcome(res any, err error) string {
	if err != nil {
		// A refusal is not a failure: the call was not allowed.
		if c := proto.CodeOf(err); c == proto.CodeForbidden || c == proto.CodeNotFound {
			return "denied"
		}
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
	s.auditEvent(ctx, audit.Event{Actor: actor, Action: action, Target: target, Outcome: outcome})
}

func (s *CallServer) auditEvent(ctx context.Context, ev audit.Event) {
	if s.cfg.Auditor == nil {
		return
	}
	if err := s.cfg.Auditor.Record(context.WithoutCancel(ctx), ev); err != nil {
		s.log.Error("workers: audit write failed", "action", ev.Action, "error", err.Error())
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
