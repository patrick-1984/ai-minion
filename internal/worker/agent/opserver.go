package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Limits and timeouts of the op server.
const (
	// DefaultMaxOps bounds the op streams served at the same time. A stream
	// beyond it is refused with rate_limited, so a flood of requests cannot
	// exhaust the Worker.
	DefaultMaxOps = 64
	// DefaultRequestTimeout bounds the wait for the op.request frame after the
	// Hub opened a stream.
	DefaultRequestTimeout = 30 * time.Second
	// DefaultWriteTimeout bounds one frame write to the Hub.
	DefaultWriteTimeout = 30 * time.Second
	// DefaultExecTimeout bounds one op. The longest op is pipe_wait with its
	// ten minutes (ops.MaxPipeWaitMS); this leaves room for the rest.
	DefaultExecTimeout = 15 * time.Minute
	// DefaultMaxRefused bounds the refused streams that are still being
	// drained. A refusal ends only when the Hub completes its side of the
	// stream (or the drain timeout), so without a bound a Hub that never
	// does could pile up streams outside every other budget. When it is
	// exhausted the link is closed.
	DefaultMaxRefused = 16
	// DefaultRefuseDrain bounds the drain of one refused stream. It matches
	// the life of a closed transport stream (30 s close timeout and 5 s
	// grace), so the slot is held for as long as the stream can linger.
	DefaultRefuseDrain = 35 * time.Second
)

// OpExecutor is the executor as the op server uses it: the same one that the
// local socket and the pipe call (AD-12). *Executor implements it; tests use a
// fake.
type OpExecutor interface {
	Execute(ctx context.Context, rq Request) (json.RawMessage, error)
}

var _ OpExecutor = (*Executor)(nil)

// OpRequestFrame is the "op.request" frame the Hub sends on an op stream
// (AD-3, AD-12). Actions is the caller's effective set computed by the Hub
// for the resolved target. RequestID is optional redundancy for a write: the
// request id is part of the arguments (ops.WriteArgs), and a top-level value
// that the arguments lack is added to them.
type OpRequestFrame struct {
	Type      string          `json:"type"`
	Op        string          `json:"op"`
	RequestID string          `json:"request_id,omitempty"`
	Actions   []ops.Action    `json:"actions"`
	Args      json.RawMessage `json:"args,omitempty"`
	// Origin is the Hub-derived provenance of the request (protocol 2.0). A
	// message-class op without a valid one is refused; see [OpServer.parse].
	// It is kept raw so that one of the wrong JSON type is refused as a
	// malformed origin, not as an unreadable frame.
	Origin json.RawMessage `json:"origin,omitempty"`
}

// OpProgressFrame is an "op.progress" frame: a short status of a long op.
// Message is English text and never terminal content.
type OpProgressFrame struct {
	Type    string          `json:"type"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// OpResultFrame is the "op.result" frame, the one successful end of an op
// stream. Result is the JSON of the op's result type (ops catalog).
type OpResultFrame struct {
	Type   string          `json:"type"`
	Result json.RawMessage `json:"result"`
}

// ErrorDetailReason is the key in the details of an "error" frame that holds
// the more specific reason code (for example preview_only under forbidden).
const ErrorDetailReason = "reason"

// OpStream is one accepted op stream as the server uses it. *transport.Stream
// implements it; tests use a fake.
type OpStream interface {
	ReadFrame() (json.RawMessage, error)
	WriteFrame(v any) error
	Close() error
	Reset()
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

var _ OpStream = (*transport.Stream)(nil)

// OpServerConfig configures an [OpServer]. Executor is required.
type OpServerConfig struct {
	Executor OpExecutor
	// Policy is the local policy that the request's actions are intersected
	// with. Nil leaves the actions as the Hub sent them (apart from dropping
	// unknown ones); the executor enforces the policy again either way.
	Policy *Policy
	// MaxOps, MaxRefused, RequestTimeout, WriteTimeout, ExecTimeout and
	// RefuseDrain override the defaults when positive.
	MaxOps         int
	MaxRefused     int
	RequestTimeout time.Duration
	WriteTimeout   time.Duration
	ExecTimeout    time.Duration
	RefuseDrain    time.Duration
	// Logger receives operational messages: never terminal content, message
	// bodies or secrets (AD-20). Nil discards them.
	Logger *slog.Logger
}

// OpServer serves the op streams of Hub links through the executor (AD-3,
// AD-12): per stream it reads one op.request, executes it with the actions of
// the request intersected with the local policy, writes op.progress frames
// the op reports, then exactly one op.result or one error frame, and closes
// the stream. Every rule of the action contract is enforced by the executor;
// the server adds none and removes none.
//
// Writes are never abandoned when the link is lost or the Hub resets the
// stream: a write that already started finishes and its result stays in the
// result cache (AD-15), so that the Hub's duplicate after the reconnect is
// answered from it. Reads end with the link and when the Hub abandons the op
// stream (reset, close or any frame after the request), so an abandoned long
// read does not hold one of the [OpServerConfig.MaxOps] slots. In both cases
// the Hub sees the lost stream as outcome unknown.
//
// The admission budget belongs to the server, not to a link: a write that
// outlives its link keeps its slot, so a reconnect does not grant a fresh
// allowance on top of it. [OpServer.Stop] ends admission and waits for the
// operations in flight, so that the store is not closed under a write.
type OpServer struct {
	cfg OpServerConfig
	log *slog.Logger

	sem       chan struct{} // MaxOps slots, held until an op ends
	refuseSem chan struct{} // MaxRefused slots, held until a refused stream is gone

	stopCtx    context.Context // ends with Stop; cancels reads
	stopCancel context.CancelFunc

	mu       sync.Mutex
	stopped  bool
	inflight sync.WaitGroup
}

// NewOpServer validates cfg and returns an op server.
func NewOpServer(cfg OpServerConfig) (*OpServer, error) {
	if cfg.Executor == nil {
		return nil, errors.New("agent: the op server needs an executor")
	}
	if cfg.MaxOps <= 0 {
		cfg.MaxOps = DefaultMaxOps
	}
	if cfg.MaxRefused <= 0 {
		cfg.MaxRefused = DefaultMaxRefused
	}
	if cfg.RefuseDrain <= 0 {
		cfg.RefuseDrain = DefaultRefuseDrain
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultExecTimeout
	}
	s := &OpServer{
		cfg: cfg, log: cfg.Logger,
		sem: make(chan struct{}, cfg.MaxOps), refuseSem: make(chan struct{}, cfg.MaxRefused),
	}
	s.stopCtx, s.stopCancel = context.WithCancel(context.Background())
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s, nil
}

// Serve accepts the op streams of one link until the link ends and returns
// when every stream of this link is done. It is a [LinkHandler].
func (s *OpServer) Serve(l *Link) {
	ctx := l.Context()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		st, err := l.Session.Accept(ctx)
		if err != nil {
			return
		}
		if st.Kind() != transport.KindOp {
			// The session only delivers kinds the Hub may open; anything else
			// is a protocol breach that is not served.
			st.Reset()
			continue
		}
		if why := s.admit(); why != nil {
			s.log.Warn("refused an op stream", "limit", s.cfg.MaxOps, "reason", why.Message)
			select {
			case s.refuseSem <- struct{}{}:
				go func() {
					defer func() { <-s.refuseSem }()
					s.refuse(st, why)
				}()
			default:
				// The Hub keeps opening streams that it does not finish:
				// that is abuse, not load. End the connection; the
				// connector reconnects.
				s.log.Warn("too many refused op streams are still open; closing the link", "limit", s.cfg.MaxRefused)
				st.Reset()
				_ = l.Session.Close()
				return
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.release()
			s.ServeStream(ctx, st)
		}()
	}
}

// admit takes one op slot. It returns nil on success (the caller must call
// release) and the error to answer with when the server is full or stopped.
func (s *OpServer) admit() *proto.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return proto.NewError(proto.CodeRateLimited, "the Worker is shutting down")
	}
	select {
	case s.sem <- struct{}{}:
		s.inflight.Add(1)
		return nil
	default:
		return proto.NewError(proto.CodeRateLimited, "the Worker serves too many operations at once")
	}
}

func (s *OpServer) release() {
	<-s.sem
	s.inflight.Done()
}

// Stop ends the admission of new op streams, cancels the reads in flight and
// waits for the writes in flight (they are never cut off, AD-15) until ctx
// ends. It is idempotent. The module calls it before the host and the store
// close, so that an accepted write finishes against an open store.
func (s *OpServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.stopCancel()
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("op streams still running: %w", ctx.Err())
	}
}

// refuse answers a stream that is not served with an error frame and holds
// the caller's refusal slot until the stream is gone: the Hub completes its
// side, or the drain timeout ends and the stream is reset.
func (s *OpServer) refuse(st OpStream, e *proto.Error) {
	_ = st.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	if st.WriteFrame(e.Frame()) == nil {
		_ = st.Close()
		_ = st.SetReadDeadline(time.Now().Add(s.cfg.RefuseDrain))
		for {
			if _, err := st.ReadFrame(); err != nil {
				break
			}
		}
	}
	st.Reset()
}

// ServeStream serves one op stream: the frame order is op.request in, then
// op.progress*, then exactly one op.result or error out, then the stream is
// closed. linkCtx is the context of the link; it ends when the connection is
// lost.
func (s *OpServer) ServeStream(linkCtx context.Context, st OpStream) {
	_ = st.SetReadDeadline(time.Now().Add(s.cfg.RequestTimeout))
	body, err := st.ReadFrame()
	if err != nil {
		// A malformed or oversized frame has reset the stream already; a
		// stream that ended or stalled before a request has nobody to answer.
		s.log.Debug("op stream ended before a request", "error", err.Error())
		st.Reset()
		return
	}
	_ = st.SetReadDeadline(time.Time{})

	out := &opOutput{st: st, timeout: s.cfg.WriteTimeout}
	if typ, _ := proto.FrameType(body); typ == proto.TypeShareProbe {
		if s.expired(linkCtx) {
			st.Reset()
			return
		}
		s.serveShareProbe(linkCtx, out, body)
		return
	}
	rq, held, perr := s.parse(body)
	if perr != nil {
		out.fail(perr)
		return
	}
	// Op admission recheck (section 11.3): the request was decoded before the
	// Hub certificate's not_after, but it is admitted only if both clocks are
	// still before it. The stream is reset; the Hub sees the outcome as unknown
	// and nothing was started.
	if s.expired(linkCtx) {
		st.Reset()
		return
	}

	write := false
	if op, ok := ops.Lookup(rq.Op); ok && op.Scope == ops.ScopeWorker {
		// A change of state that is not a Write op (share_set) is not cut off by
		// the loss of the link either: it is committed or it is not, and the
		// Hub learns the rule in force from the event stream on reconnect.
		write = op.ChangesState()
	}
	// A write is not cut off by the loss of the link: whatever it does to the
	// pane is recorded in the result cache either way.
	base := linkCtx
	if write {
		base = context.WithoutCancel(linkCtx)
	}
	ctx, cancel := context.WithTimeout(base, s.cfg.ExecTimeout)
	defer cancel()
	if !write {
		// A read also ends when the server stops.
		defer context.AfterFunc(s.stopCtx, cancel)()
	}
	ctx = WithProgress(ctx, out.progress)

	// The Hub resets the stream when its caller goes away and sends nothing
	// after the request, so any read result before the final frame means the
	// op is abandoned: a read is cancelled to free its slot, a write is not
	// (AD-15). out.end, called before the final frame, stops the watcher.
	var ended, abandoned atomic.Bool
	out.end = func() {
		ended.Store(true)
		_ = st.SetReadDeadline(time.Now())
	}
	go func() {
		_, _ = st.ReadFrame()
		if ended.Load() {
			return
		}
		abandoned.Store(true)
		if !write {
			s.log.Debug("the Hub abandoned a read; cancelled it", "op", rq.Op)
			cancel()
		}
	}()
	defer out.end()

	raw, err := s.cfg.Executor.Execute(ctx, rq)
	if err != nil {
		if linkCtx.Err() != nil || (abandoned.Load() && !write) {
			// The Hub is gone and sees the stream as lost (outcome unknown).
			s.log.Debug("op ended with the link or the stream", "op", rq.Op)
			out.end()
			st.Reset()
			return
		}
		out.fail(s.errorOf(rq, held, err))
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("null")
	}
	out.result(raw)
}

// expired reads both clocks of the link's certificate and reports whether the
// connection is over (closing it the first time).
func (s *OpServer) expired(linkCtx context.Context) bool {
	if guardFrom(linkCtx).check() {
		s.log.Info("an operation was not admitted: the Hub certificate reached its not_after")
		return true
	}
	return false
}

// serveShareProbe answers the Hub's internal share probe on this stream: one
// op.result with the rule in force and the barrier (see ops.ShareProbe). It is
// not an op and needs no action; only the Hub's own connection reaches it. An
// executor that cannot answer it refuses it, and the Hub then treats the share
// rule of this connection as not confirmed.
func (s *OpServer) serveShareProbe(linkCtx context.Context, out *opOutput, body json.RawMessage) {
	pr, ok := s.cfg.Executor.(ShareProber)
	if !ok {
		out.fail(proto.NewError(proto.CodeUnsupportedOp, "the Worker does not answer the share probe"))
		return
	}
	var f ops.ShareProbeRequest
	if err := proto.DecodeFrame(body, &f); err != nil {
		out.fail(errorAsProto(err))
		return
	}
	ctx, cancel := context.WithTimeout(linkCtx, s.cfg.RequestTimeout)
	defer cancel()
	res, err := pr.ShareProbe(ctx, f.OpBarrier)
	if err != nil {
		out.fail(s.errorOf(Request{Op: "share_probe"}, nil, err))
		return
	}
	raw, err := json.Marshal(res)
	if err != nil {
		out.fail(proto.NewError(proto.CodeInternal, "the operation failed"))
		return
	}
	out.result(raw)
}

// parse reads the op.request frame and applies the local policy to its
// actions.
func (s *OpServer) parse(body json.RawMessage) (rq Request, held []ops.Action, _ *proto.Error) {
	if typ, _ := proto.FrameType(body); typ != proto.TypeOpRequest {
		return Request{}, nil, proto.NewError(proto.CodeInvalidArgument, "the first frame of an op stream must be op.request")
	}
	var f OpRequestFrame
	if err := proto.DecodeFrame(body, &f); err != nil {
		return Request{}, nil, errorAsProto(err)
	}
	if f.Op == "" {
		return Request{}, nil, proto.NewError(proto.CodeInvalidArgument, "op.request has no op")
	}
	op, known := ops.Lookup(f.Op)
	if known && op.Scope != ops.ScopeWorker {
		known = false
	}
	args := f.Args
	if known && op.Write && f.RequestID != "" {
		var err error
		if args, err = withRequestID(args, f.RequestID); err != nil {
			return Request{}, nil, errorAsProto(err)
		}
	}
	origin, perr := s.checkOrigin(f)
	if perr != nil {
		return Request{}, nil, perr
	}
	held = IntersectActions(f.Actions, nil)
	eff := IntersectActions(f.Actions, s.cfg.Policy)
	return Request{Op: f.Op, Actions: eff, Args: args, Origin: origin}, held, nil
}

// OriginRefusalAuditor is the executor as the op server uses it to leave the
// local audit row of a request refused for its origin. *Executor implements it.
type OriginRefusalAuditor interface {
	AuditOriginRefusal(ctx context.Context, op string, args json.RawMessage, origin *proto.Origin, reason string)
}

// checkOrigin enforces the origin rules of protocol 2.0 (AD-12, F-01): every
// Hub op.request needs a well formed origin, whatever the op; for a
// message-class op (ops.IsMessageClass), when it names a source Worker other
// than this Worker, the local pipe_accept_from policy must accept it. A refusal
// is audited with the origin and answered pipe_origin_refused; nothing reaches
// a pane. The args of the request never take part: the origin is a member of
// the frame that only the Hub's op.request carries.
func (s *OpServer) checkOrigin(f OpRequestFrame) (*proto.Origin, *proto.Error) {
	message := ops.IsMessageClass(f.Op)
	refuse := func(reason string, origin *proto.Origin, err error) (*proto.Origin, *proto.Error) {
		if a, ok := s.cfg.Executor.(OriginRefusalAuditor); ok {
			a.AuditOriginRefusal(context.WithoutCancel(s.stopCtx), f.Op, f.Args, origin, reason)
		}
		var pe *proto.Error
		if errors.As(err, &pe) {
			return nil, pe
		}
		return nil, proto.NewError(proto.CodePipeOriginRefused, reason)
	}
	var origin *proto.Origin
	if t := bytes.TrimSpace(f.Origin); len(t) > 0 && string(t) != "null" {
		var o proto.Origin
		dec := json.NewDecoder(bytes.NewReader(t))
		if err := dec.Decode(&o); err != nil || dec.More() {
			return refuse("malformed_origin", nil, proto.NewError(proto.CodePipeOriginRefused, "malformed origin"))
		}
		if err := o.Validate(); err != nil {
			return refuse("malformed_origin", nil, err)
		}
		origin = &o
	}
	if origin == nil {
		return refuse("missing_origin", nil, proto.NewError(proto.CodePipeOriginRefused, "the request carries no origin; the Worker refuses a Hub request without one"))
	}
	if !message {
		// pipe_accept_from governs messages only.
		return origin, nil
	}
	if err := s.cfg.Policy.CheckOrigin(*origin); err != nil {
		return refuse("not_accepted", origin, err)
	}
	return origin, nil
}

// IntersectActions intersects the actions a caller holds with the local
// policy (AD-12: the local policy wins whatever the Hub sends). Unknown
// actions are dropped; with a preview-only policy the actions operate and
// message are dropped too, because a preview-only Worker takes no input.
func IntersectActions(held []ops.Action, p *Policy) []ops.Action {
	previewOnly := p != nil && p.PreviewOnly()
	out := make([]ops.Action, 0, len(held))
	for _, a := range held {
		if !a.Valid() {
			continue
		}
		if previewOnly && (a == ops.ActionOperate || a == ops.ActionMessage) {
			continue
		}
		out = append(out, a)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// withRequestID adds the top-level request id to the arguments of a write.
// An id that the arguments carry as well must be the same.
func withRequestID(args json.RawMessage, id string) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if t := bytes.TrimSpace(args); len(t) > 0 && string(t) != "null" {
		if err := json.Unmarshal(t, &m); err != nil || m == nil {
			return nil, proto.NewError(proto.CodeInvalidArgument, "the arguments must be a JSON object")
		}
	}
	want, _ := json.Marshal(id)
	if have, ok := m["request_id"]; ok {
		if !bytes.Equal(bytes.TrimSpace(have), want) {
			return nil, proto.NewError(proto.CodeInvalidArgument, "request_id differs from the request id in the arguments")
		}
		return args, nil
	}
	m["request_id"] = want
	b, err := json.Marshal(m)
	if err != nil {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the arguments cannot be encoded")
	}
	return b, nil
}

// errorOf maps an executor error to the error frame. Only an *APIError and
// its code, reason and message reach the Hub; the cause stays in the log.
// held is what the Hub's request granted before the local policy was applied.
func (s *OpServer) errorOf(rq Request, held []ops.Action, err error) *proto.Error {
	if ae, ok := AsAPIError(err); ok {
		e := &proto.Error{Code: ae.Code, Message: ae.Message}
		reason := ae.Reason
		if ae.Code == proto.CodeForbidden && reason == "" && s.restrictedByPolicy(rq.Op, held) {
			// The request's own actions would have sufficed: say that the
			// local policy took them away instead of "needs the action".
			reason, e.Message = proto.CodePreviewOnly, "the Worker is preview-only"
		}
		if reason != "" {
			e.Details, _ = json.Marshal(map[string]string{ErrorDetailReason: reason.String()})
		}
		if ae.Code == proto.CodeInternal {
			s.log.Error("op failed", "op", rq.Op, "error", ae.Error())
		}
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return proto.NewError(proto.CodeInternal, "the operation timed out")
	}
	s.log.Error("op failed", "op", rq.Op, "error", err.Error())
	return proto.NewError(proto.CodeInternal, "the operation failed")
}

// restrictedByPolicy reports whether a preview-only Worker is why a caller
// that held the actions held may not run op.
func (s *OpServer) restrictedByPolicy(op string, held []ops.Action) bool {
	if s.cfg.Policy == nil || !s.cfg.Policy.PreviewOnly() {
		return false
	}
	o, ok := ops.Lookup(op)
	return ok && o.Actions.Permits(held...)
}

// errorAsProto turns an error from the frame helpers into a *proto.Error.
func errorAsProto(err error) *proto.Error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	return proto.NewError(proto.CodeInvalidArgument, "the request cannot be read")
}

// opOutput writes the frames of one op stream and guarantees the shape: any
// number of progress frames, then exactly one final frame, then nothing.
type opOutput struct {
	st      OpStream
	timeout time.Duration
	// end, when set, runs before the final frame (ServeStream stops the
	// stream watcher with it).
	end func()

	mu   sync.Mutex
	done bool
}

func (o *opOutput) write(v any) error {
	_ = o.st.SetWriteDeadline(time.Now().Add(o.timeout))
	return o.st.WriteFrame(v)
}

// progress sends an op.progress frame carrying one element of a streaming
// handler (see [EmitProgress]). It is dropped after the final frame and when
// the stream fails: progress is best effort and never decides the outcome.
func (o *opOutput) progress(data json.RawMessage) {
	f := OpProgressFrame{Type: proto.TypeOpProgress, Data: data}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	_ = o.write(f)
}

func (o *opOutput) result(raw json.RawMessage) {
	o.finish(OpResultFrame{Type: proto.TypeOpResult, Result: raw})
}

func (o *opOutput) fail(e *proto.Error) { o.finish(e.Frame()) }

// finish writes the one final frame and closes the stream. A final frame that
// cannot be written (too large, a dead stream) resets the stream: the Hub then
// sees a lost stream, never a second answer.
func (o *opOutput) finish(v any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	o.done = true
	if o.end != nil {
		o.end()
	}
	if err := o.write(v); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.CodeFrameTooLarge {
			// Tell the Hub why, in a frame that always fits.
			if o.write(proto.NewError(proto.CodeFrameTooLarge, "the result is larger than a frame").Frame()) == nil {
				_ = o.st.Close()
				return
			}
		}
		o.st.Reset()
		return
	}
	_ = o.st.Close()
}
