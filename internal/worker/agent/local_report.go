package agent

// F06: a write that a local caller (CLI, master session, MCP) makes on this
// Worker's own session through the local control socket is also reported to
// the Hub, which records it in its audit trail (proto.OpLocalAuditV1).
//
// The report comes from the executor's own audit point (Executor.record), so
// it describes what really ran: a duplicate request answered from the result
// cache is not reported again. It names the operation, the outcome, a reason
// code, the request id and, only when the executor resolved it, the target
// session; never message text, terminal content or an unresolved argument.
// It is best effort and bounded: a small queue, one sender, drops counted;
// nothing is sent without a Hub connection or to a Hub that does not offer
// the op (v0.20.1 and older).

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	localReportTimeout = 5 * time.Second
	// localReportQueue bounds the reports waiting for the Hub.
	localReportQueue = 64
)

// LocalWrite is the report of one local write that the executor ran.
type LocalWrite struct {
	Op        string
	Outcome   string
	Reason    string
	RequestID string
	// ReportID is new for each write the executor ran: the Hub drops a
	// repeated delivery of one report by it.
	ReportID string
	// Session is the session the executor resolved to an existing pane;
	// empty when it resolved none (a refusal before resolution, an error, a
	// creation whose result is unknown).
	Session string
}

// OnLocalWrite makes fn see every write of a local caller that the executor
// ran (not the duplicates it answered from its cache). fn must not block.
func (e *Executor) OnLocalWrite(fn func(LocalWrite)) {
	if fn == nil {
		e.localWrites.Store(nil)
		return
	}
	e.localWrites.Store(&fn)
}

func (e *Executor) reportLocalWrite(op string, wa ops.WriteArgs, res ops.WriteResult, err error) {
	h := e.localWrites.Load()
	if h == nil {
		return
	}
	lw := LocalWrite{Op: op, Outcome: string(res.Outcome), ReportID: ops.NewUUIDv7()}
	if ops.ValidUUIDv7(wa.RequestID) {
		lw.RequestID = wa.RequestID
	}
	switch {
	case err != nil:
		lw.Outcome, lw.Reason = "error", string(proto.CodeInternal)
		if ae, ok := AsAPIError(err); ok {
			lw.Outcome, lw.Reason = "refused", string(ae.Code)
			if ae.Reason != "" {
				lw.Reason = string(ae.Reason)
			}
		}
	default:
		lw.Reason = res.Reason
		if lw.Outcome == "" {
			lw.Outcome = string(ops.OutcomeUnknown)
		}
		// Only a session resolved to an existing pane (one with a pane id)
		// is named. An ambiguous creation carries the requested name without
		// any pane: what the caller typed is never reported.
		if res.Pane != nil && res.Pane.ID != "" && ops.ValidSessionName(res.Pane.Session) {
			lw.Session = res.Pane.Session
		}
	}
	(*h)(lw)
}

// welcomer is implemented by the call client: the welcome of the current
// Hub connection.
type welcomer interface {
	Welcome() (HubWelcome, bool)
}

// localReporter sends local-write reports to the Hub from a bounded queue.
type localReporter struct {
	calls   func() PipeCaller
	log     *slog.Logger
	queue   chan LocalWrite
	start   sync.Once
	dropped atomic.Uint64
}

func newLocalReporter(calls func() PipeCaller, log *slog.Logger) *localReporter {
	return &localReporter{calls: calls, log: log, queue: make(chan LocalWrite, localReportQueue)}
}

// hub returns the call client when a Hub connection that offers the report
// is up, or nil.
func (r *localReporter) hub() PipeCaller {
	if r.calls == nil {
		return nil
	}
	cc := r.calls()
	if cc == nil {
		return nil
	}
	w, ok := cc.(welcomer)
	if !ok {
		return nil
	}
	if wel, up := w.Welcome(); !up || !wel.LocalAuditV1 {
		return nil
	}
	return cc
}

// Enqueue queues a report without blocking; a full queue drops it.
func (r *localReporter) Enqueue(lw LocalWrite) {
	if r.hub() == nil {
		return
	}
	r.start.Do(func() { go r.run() })
	select {
	case r.queue <- lw:
	default:
		if n := r.dropped.Add(1); n == 1 || n%100 == 0 {
			r.log.Warn("local write reports to the Hub dropped: the queue is full", "dropped_total", n)
		}
	}
}

// Dropped is the number of reports dropped so far.
func (r *localReporter) Dropped() uint64 { return r.dropped.Load() }

func (r *localReporter) run() {
	for lw := range r.queue {
		cc := r.hub()
		if cc == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(withoutCallKey(context.Background()), localReportTimeout)
		err := cc.Do(ctx, proto.OpLocalAuditV1, proto.LocalAuditArgs{
			Op: lw.Op, Session: lw.Session, Outcome: lw.Outcome, Reason: lw.Reason, RequestID: lw.RequestID, ReportID: lw.ReportID,
		}, nil)
		cancel()
		if err != nil {
			r.log.Debug("cannot report a local write to the Hub", "op", lw.Op, "error", err.Error())
		}
	}
}
