package workers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Call streams of an admitted Worker (AD-3, AD-10). The Worker opens a `ca`
// stream per upstream request; the Hub reads one `op.request`, runs the op for
// the Worker identity that authenticated the connection and answers with one
// `op.result` or `error`. The only op served here is [ops.OpWorkerRetire]
// (Story 4.7, amendment A-16). This file does not read the session: the one
// accept loop of a session is the [hostedSession] one (streams.go), which
// gives the call streams to [callStreams]. Story 7.4 (call authentication, the
// pipe call stream) adds its ops with [Handler.HandleCallOp] and must keep
// worker_retire as it is: it is served only on the Worker's own stream, for the
// identity the handshake authenticated.
const (
	// callTimeout bounds the whole life of one call stream.
	callTimeout = 15 * time.Second
	// maxConcurrentCalls bounds the call streams one Worker may have in
	// flight; further ones are refused with rate_limited by the stream host
	// (admission.go).
	maxConcurrentCalls = 8
	// retireGrace is how long the Hub waits for the Worker to close the
	// session after a retirement before it closes it itself.
	retireGrace = 2 * time.Second
)

// RemoveWorkerFunc removes the enrollment of a Worker and leaves a tombstone
// (AD-9). The Service default is [store.Store.TombstoneWorker]; the Hub module
// replaces it through [Service.SetRemoveWorker] with the removal of Story 4.10
// (bound API keys, trust rows, tombstone and audit record in one transaction,
// without the live revoke: the call ends the connection itself). It returns
// [store.ErrWorkerNotFound] for an unknown id, which the call treats as
// already removed.
type RemoveWorkerFunc func(ctx context.Context, workerID, removedBy string, removedAt int64) error

// callRequest is the `op.request` frame of a call stream.
//
// request_id and api_key are the members of the pipe ops of Story 7.4
// ([CallServer]): the key, when present, is the credential of the caller (a
// Worker-bound API key); it is never logged, audited or echoed.
type callRequest struct {
	Type      string          `json:"type"`
	Op        string          `json:"op"`
	RequestID string          `json:"request_id,omitempty"`
	Args      json.RawMessage `json:"args"`
	APIKey    string          `json:"api_key,omitempty"`
}

// callStreams is the [StreamHandler] of the call streams (KindCall) of one
// session. It is registered by [Handler.Welcome] before the Worker goes online
// and does not read the session itself: the accept loop of the session's
// [streamHost] gives it the streams.
type callStreams struct {
	h    *Handler
	w    store.WorkerRow
	sess callSession
	wg   sync.WaitGroup
}

// callSession is what a call needs of the Worker's session.
type callSession interface {
	Close() error
}

func newCallStreams(h *Handler, w store.WorkerRow, sess callSession) *callStreams {
	return &callStreams{h: h, w: w, sess: sess}
}

// Serve implements [StreamHandler]. The call limit of the Worker is the one
// of the stream host's call lane: a stream that arrives here has a slot.
func (c *callStreams) Serve(ctx context.Context, st *transport.Stream, done func()) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if done != nil {
			defer done()
		}
		c.h.serveCall(ctx, c.w, c.sess, st)
	}()
}

// Close implements [StreamHandler].
func (c *callStreams) Close() { c.wg.Wait() }

func (h *Handler) refuseCall(st *transport.Stream, e *proto.Error) {
	_ = st.SetWriteDeadline(time.Now().Add(controlTimeout))
	_ = st.WriteFrame(e.Frame())
	_ = st.Close()
}

// serveCall serves one call stream.
func (h *Handler) serveCall(ctx context.Context, w store.WorkerRow, sess callSession, st *transport.Stream) {
	_ = st.SetDeadline(time.Now().Add(callTimeout))
	// An idle call stream must not outlive the session or the Hub: the end of
	// ctx interrupts a read that waits for the request, and the read of a
	// running op (the caller going away, see [CallServer]).
	defer interruptOnDone(ctx, st)()
	defer func() { _ = st.Close() }()
	body, err := st.ReadFrame()
	if err != nil {
		st.Reset() // no request in time, the Worker went away or ctx ended: abandon it
		return
	}
	var rq callRequest
	if err := json.Unmarshal(body, &rq); err != nil || rq.Type != proto.TypeOpRequest {
		h.refuseCall(st, proto.NewError(proto.CodeInvalidArgument, "expected an op.request frame"))
		return
	}
	if rq.Op == ops.OpWorkerRetire {
		h.retireWorker(ctx, w, sess, st, rq.Args)
		return
	}
	// F-02: a Worker that waits for approval may retire itself (above) and
	// nothing else. The row is read again, so a refusal is never stale.
	if err := h.requireApproved(ctx, w); err != nil {
		h.refuseCall(st, err)
		return
	}
	if fn := h.callOp(rq.Op); fn != nil {
		// The request was read inside callTimeout. What follows is the op's:
		// an op that streams (pipe_wait) outlives it, and bounds itself with
		// its own write deadlines, its timeout and ctx, which ends with the
		// session and, with the AfterFunc above, resets the stream.
		_ = st.SetDeadline(time.Time{})
		fn(ctx, Call{Worker: w, Stream: st, Args: rq.Args, RequestID: rq.RequestID, APIKey: rq.APIKey})
		return
	}
	h.refuseCall(st, proto.NewError(proto.CodeUnsupportedOp, "the Hub does not serve this operation on a call stream"))
}

// requireApproved refuses the call of a Worker that is still pending. A Worker
// that was approved when it connected needs no check (approval is never
// undone); one that was pending is checked against the row.
func (h *Handler) requireApproved(ctx context.Context, w store.WorkerRow) *proto.Error {
	if !w.Pending {
		return nil
	}
	cur, err := h.cfg.Repository.WorkerByPub(ctx, w.Pub)
	switch {
	case err != nil:
		h.log.Warn("worker call: cannot read the approval state; the call is refused", "worker_id", w.ID, "error", err.Error())
		return proto.NewError(proto.CodeWorkerPending, "the Worker is waiting for approval by its owner or an admin")
	case cur.Pending:
		return proto.NewError(proto.CodeWorkerPending, "the Worker is waiting for approval by its owner or an admin")
	}
	return nil
}

// Call is one call stream whose op.request was read, handed to a [CallOp].
type Call struct {
	// Worker is the row of the Worker the handshake authenticated: the
	// identity of the call, never a name from the request.
	Worker store.WorkerRow
	// Stream is the call stream. The op answers on it with one op.result or
	// error frame; the call loop closes it afterwards. Its deadlines are
	// cleared once the op.request was read: the op sets its own.
	Stream *transport.Stream
	// Args are the raw arguments of the op.request.
	Args json.RawMessage
	// RequestID and APIKey are the request_id and the optional api_key of the
	// op.request. The key is a credential: never log or echo it.
	RequestID string
	APIKey    string
}

// CallOp serves one op of the call streams. It runs on the goroutine of the
// call, bounded by the per-connection call limit and the call timeout; ctx ends
// with the session.
type CallOp func(ctx context.Context, c Call)

// HandleCallOp registers the serving of op on call streams (Story 7.4 and
// later). worker_retire belongs to this package and cannot be replaced; it
// reports false for it, for a nil fn and for an op that is already
// registered. It is safe to call while connections are served.
func (h *Handler) HandleCallOp(op string, fn CallOp) bool {
	if fn == nil || op == "" || op == ops.OpWorkerRetire {
		return false
	}
	h.callMu.Lock()
	defer h.callMu.Unlock()
	if _, ok := h.callOps[op]; ok {
		return false
	}
	if h.callOps == nil {
		h.callOps = map[string]CallOp{}
	}
	h.callOps[op] = fn
	return true
}

func (h *Handler) callOp(op string) CallOp {
	h.callMu.RLock()
	defer h.callMu.RUnlock()
	return h.callOps[op]
}

// retireWorker removes the enrollment of the Worker that authenticated the
// connection and leaves a tombstone, so every later connect with its key gets
// the signed revoke (AD-9). It acts on w, the row the connection was admitted
// with, never on a name from the request. Removing an already removed Worker
// succeeds: the goal is reached.
//
// Authorization: the decision is the KEEP guard retire_self_only on the
// identity the handshake authenticated; there is no authz.Decide call, because
// the op names no Worker and is served only on a Worker's own call stream.
// Story 7.4's call authentication must keep it that way.
func (h *Handler) retireWorker(ctx context.Context, w store.WorkerRow, sess callSession, st *transport.Stream, raw json.RawMessage) {
	var args ops.WorkerRetireArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			h.refuseCall(st, proto.NewError(proto.CodeInvalidArgument, "the arguments of worker_retire are not valid"))
			return
		}
	}
	if err := args.Validate(); err != nil {
		h.refuseCall(st, proto.NewError(proto.CodeInvalidArgument, err.Error()))
		return
	}
	remove, audited := h.removal()
	if remove == nil {
		h.refuseCall(st, proto.NewError(proto.CodeUnsupportedOp, "this Hub cannot remove Workers"))
		return
	}
	actor := "worker:" + w.ID
	err := remove(ctx, w.ID, actor, time.Now().UnixMilli())
	switch {
	case err == nil, errors.Is(err, store.ErrWorkerNotFound):
	default:
		h.log.Error("worker retire: cannot remove the Worker", "worker_id", w.ID, "error", err.Error())
		h.refuseCall(st, proto.NewError(proto.CodeInternal, ""))
		return
	}
	// A removal that audits writes its record inside its transaction; a second
	// best-effort record here would double it and could outlive a rollback.
	if a := h.cfg.Auditor; a != nil && !audited {
		if err := a.Record(ctx, audit.Event{
			Actor: actor, Action: ops.OpWorkerRetire, Target: w.ID, Outcome: string(ops.OutcomeDelivered),
		}); err != nil {
			h.log.Warn("worker retire: cannot audit the retirement", "worker_id", w.ID, "error", err.Error())
		}
	}
	h.log.Info("worker retired its own enrollment", "worker_id", w.ID, "name", w.Name)
	_ = st.SetWriteDeadline(time.Now().Add(controlTimeout))
	if err := st.WriteFrame(map[string]any{
		"type":   proto.TypeOpResult,
		"result": ops.WriteResult{Outcome: ops.OutcomeDelivered},
	}); err != nil {
		h.log.Debug("worker retire: cannot deliver the answer", "worker_id", w.ID, "error", err.Error())
	}
	_ = st.Close()
	// The enrollment is gone: no connection may stay online as that Worker.
	// That includes one the registry holds in place of sess: a second
	// connection with the old key can have been admitted before the removal
	// committed and replaced this session. A removal that went through
	// [Service.SetRemoveWorker] does not revoke live connections itself, so
	// the registry connection is ended here, now and again after the grace
	// (a connect that was admitted before the commit may register late). The
	// Worker normally closes its session when it has the answer; the grace
	// is the bound for one that does not.
	h.cfg.Registry.Disconnect(w.ID)
	time.AfterFunc(retireGrace, func() {
		_ = sess.Close()
		h.cfg.Registry.Disconnect(w.ID)
	})
}
