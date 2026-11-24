package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// localAuditTimeout bounds the write of one reported event.
const localAuditTimeout = 5 * time.Second

// AuditLocalOp is the audit action prefix of a write made through a Worker's
// local control socket: "local.send_text" and so on.
const AuditLocalOp = "local."

// Limits of the local-write reports of one Worker. A report is small (an op
// name, an outcome, a reason code, a request id and a session name); the
// budget lets a busy Worker report a burst of writes and then about one a
// second, and drops (and counts) the rest, so one Worker cannot flood the
// audit trail or its log sinks.
const (
	localAuditMaxBytes = 1024
	localAuditBurst    = 120
	localAuditPerSec   = 1.0
	// localAuditSeen is how many recent request ids of a Worker are kept to
	// drop a repeated report of the same write.
	localAuditSeen = 256
)

// localAuditOutcomes are the outcomes a report may carry: the write outcomes
// of the catalog, and refused or error for a write that ended as an error.
var localAuditOutcomes = map[string]bool{
	string(ops.OutcomeDelivered): true, string(ops.OutcomeFailedNotSent): true,
	string(ops.OutcomePartial): true, string(ops.OutcomeUnknown): true,
	"refused": true, "error": true,
}

// LocalAudit serves proto.OpLocalAuditV1: it turns the report of a Worker
// about a local write on its own session into an audit event. The actor is
// the Worker's own local entry point ("worker:<id>/local"): the Worker is
// authenticated by its connection, never by the report, and the calling
// session a Worker could name is not verified, so it is not recorded.
type LocalAudit struct {
	auditor *audit.Log
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	workers map[string]*localAuditBudget
}

type localAuditBudget struct {
	tokens   float64
	last     time.Time
	seen     map[string]struct{}
	order    []string
	dropped  uint64
	lastWarn time.Time
}

// NewLocalAudit returns the service that records local-operation reports.
func NewLocalAudit(a *audit.Log) *LocalAudit {
	return &LocalAudit{auditor: a, log: slog.Default(), now: time.Now, workers: map[string]*localAuditBudget{}}
}

// WithLogger sets the logger of dropped-report warnings.
func (l *LocalAudit) WithLogger(lg *slog.Logger) *LocalAudit {
	if lg != nil {
		l.log = lg
	}
	return l
}

// Bind registers the call op on service.
func (l *LocalAudit) Bind(service *Service) error {
	if !service.HandleCallOp(proto.OpLocalAuditV1, l.serve) {
		return errors.New("workers: the local audit report is already registered")
	}
	return nil
}

// Dropped returns the number of reports of workerID dropped by the budget.
func (l *LocalAudit) Dropped(workerID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.workers[workerID]; b != nil {
		return b.dropped
	}
	return 0
}

func (l *LocalAudit) serve(ctx context.Context, c Call) {
	answer := func(err *proto.Error) {
		_ = c.Stream.SetWriteDeadline(time.Now().Add(controlTimeout))
		if err != nil {
			_ = c.Stream.WriteFrame(err.Frame())
			return
		}
		_ = c.Stream.WriteFrame(OpResult{Type: proto.TypeOpResult, Result: json.RawMessage(`{}`)})
	}
	if len(c.Args) > localAuditMaxBytes {
		answer(proto.NewError(proto.CodeInvalidArgument, "the local audit report is too large"))
		return
	}
	var a proto.LocalAuditArgs
	if err := json.Unmarshal(c.Args, &a); err != nil {
		answer(proto.NewError(proto.CodeInvalidArgument, "invalid local audit report"))
		return
	}
	ev, err := localAuditEvent(c.Worker.ID, a)
	if err != nil {
		answer(proto.NewError(proto.CodeInvalidArgument, err.Error()))
		return
	}
	switch l.admit(c.Worker.ID, a) {
	case admitDuplicate:
		answer(nil) // already recorded: a retry of the same report
		return
	case admitDropped:
		answer(proto.NewError(proto.CodeRateLimited, "too many local audit reports; this one was dropped"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), localAuditTimeout)
	defer cancel()
	if err := l.auditor.Record(ctx, ev); err != nil {
		answer(proto.NewError(proto.CodeInternal, ""))
		return
	}
	answer(nil)
}

type admitResult int

const (
	admitOK admitResult = iota
	admitDuplicate
	admitDropped
)

// admit applies the per-Worker budget and drops a report already seen (same
// request id, outcome and reason from the same Worker).
func (l *LocalAudit) admit(workerID string, a proto.LocalAuditArgs) admitResult {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.workers[workerID]
	if b == nil {
		b = &localAuditBudget{tokens: localAuditBurst, last: now, seen: map[string]struct{}{}}
		l.workers[workerID] = b
	}
	key := ""
	if a.RequestID != "" {
		key = a.RequestID + "|" + a.Outcome + "|" + a.Reason
		if _, ok := b.seen[key]; ok {
			return admitDuplicate
		}
	}
	if d := now.Sub(b.last).Seconds(); d > 0 {
		b.tokens = min(localAuditBurst, b.tokens+d*localAuditPerSec)
	}
	b.last = now
	if b.tokens < 1 {
		b.dropped++
		if now.Sub(b.lastWarn) >= time.Minute {
			b.lastWarn = now
			l.log.Warn("local audit reports of a Worker dropped: over its budget", "worker_id", workerID, "dropped_total", b.dropped)
		}
		return admitDropped
	}
	b.tokens--
	if key != "" {
		b.seen[key] = struct{}{}
		b.order = append(b.order, key)
		if len(b.order) > localAuditSeen {
			delete(b.seen, b.order[0])
			b.order = b.order[1:]
		}
	}
	return admitOK
}

// localAuditEvent validates a report and builds its event. Only a state
// changing Worker operation of the catalog, a known outcome and a known
// reason code are accepted, and the session must be a valid session name, so
// a report cannot fake another kind of event or carry free text.
func localAuditEvent(workerID string, a proto.LocalAuditArgs) (audit.Event, error) {
	op, ok := ops.Lookup(a.Op)
	if !ok || op.Scope != ops.ScopeWorker || !op.ChangesState() {
		return audit.Event{}, errors.New("not a local write operation")
	}
	if !localAuditOutcomes[a.Outcome] {
		return audit.Event{}, errors.New("unknown outcome")
	}
	if a.Reason != "" && !proto.Code(a.Reason).Known() {
		return audit.Event{}, errors.New("unknown reason code")
	}
	if a.RequestID != "" && !ops.ValidUUIDv7(a.RequestID) {
		return audit.Event{}, errors.New("request_id must be a UUIDv7")
	}
	if a.Session != "" && !ops.ValidSessionName(a.Session) {
		return audit.Event{}, errors.New("invalid session name")
	}
	target := workerID
	if a.Session != "" {
		target += ":" + a.Session
	}
	outcome := a.Outcome
	if a.Reason != "" {
		outcome += ":" + a.Reason
	}
	ev := audit.Event{Actor: "worker:" + workerID + "/local", Action: AuditLocalOp + a.Op, Target: target, Outcome: outcome}
	if err := ev.Validate(); err != nil {
		return audit.Event{}, err
	}
	return ev, nil
}
