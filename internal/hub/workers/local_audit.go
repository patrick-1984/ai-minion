package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
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
	// localAuditSeen is how many recent report ids of a Worker are kept to
	// drop a repeated delivery of the same report.
	localAuditSeen = 256
	// localAuditIdle is how long the budget of a Worker that sends nothing
	// is kept: long enough to survive a reconnect, short enough that retired
	// Workers do not accumulate.
	localAuditIdle = time.Hour
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
	auditor localAuditRecorder
	log     *slog.Logger
	now     func() time.Time

	mu        sync.Mutex
	workers   map[string]*localAuditBudget
	lastSweep time.Time
}

// localAuditRecorder is the part of *audit.Log the service uses.
type localAuditRecorder interface {
	Record(ctx context.Context, ev audit.Event) error
}

type localAuditBudget struct {
	tokens float64
	last   time.Time
	// seen holds the report ids recorded (true) or being recorded (false).
	seen     map[string]bool
	order    []string
	dropped  uint64
	lastWarn time.Time
}

// NewLocalAudit returns the service that records local-operation reports.
func NewLocalAudit(a *audit.Log) *LocalAudit {
	l := &LocalAudit{log: slog.Default(), now: time.Now, workers: map[string]*localAuditBudget{}}
	if a != nil {
		l.auditor = a
	}
	return l
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
	perr := l.handle(ctx, c.Worker.ID, c.Args)
	_ = c.Stream.SetWriteDeadline(time.Now().Add(controlTimeout))
	if perr != nil {
		_ = c.Stream.WriteFrame(perr.Frame())
		return
	}
	_ = c.Stream.WriteFrame(OpResult{Type: proto.TypeOpResult, Result: json.RawMessage(`{}`)})
}

// handle validates and records one report of workerID; nil means recorded
// (now or by an earlier delivery of the same report).
func (l *LocalAudit) handle(ctx context.Context, workerID string, raw json.RawMessage) *proto.Error {
	if len(raw) > localAuditMaxBytes {
		return proto.NewError(proto.CodeInvalidArgument, "the local audit report is too large")
	}
	var a proto.LocalAuditArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return proto.NewError(proto.CodeInvalidArgument, "invalid local audit report")
	}
	ev, err := localAuditEvent(workerID, a)
	if err != nil {
		return proto.NewError(proto.CodeInvalidArgument, err.Error())
	}
	switch l.admit(workerID, a.ReportID) {
	case admitDuplicate:
		return nil // already recorded: a repeated delivery of the same report
	case admitInProgress:
		return proto.NewError(proto.CodeConflict, "this report is being recorded; send it again later")
	case admitDropped:
		return proto.NewError(proto.CodeRateLimited, "too many local audit reports; this one was dropped")
	}
	if l.auditor == nil {
		l.settle(workerID, a.ReportID, false)
		return proto.NewError(proto.CodeInternal, "")
	}
	// The log record of the event carries the id of the write it describes,
	// not the id of the HTTP request that opened the Worker connection (which
	// every write on that connection would share): the write's request id,
	// or the report id when the write has none.
	id := a.RequestID
	if id == "" {
		id = a.ReportID
	}
	ctx, cancel := context.WithTimeout(audit.WithRequestID(context.WithoutCancel(ctx), id), localAuditTimeout)
	defer cancel()
	err = l.auditor.Record(ctx, ev)
	l.settle(workerID, a.ReportID, err == nil)
	if err != nil {
		return proto.NewError(proto.CodeInternal, "")
	}
	return nil
}

type admitResult int

const (
	admitOK admitResult = iota
	admitDuplicate
	admitInProgress
	admitDropped
)

// admit applies the per-Worker budget and reserves the report id: a report
// already recorded is a duplicate, one being recorded is in progress. The
// caller settles every admitted report with [LocalAudit.settle].
func (l *LocalAudit) admit(workerID, reportID string) admitResult {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b := l.workers[workerID]
	if b == nil {
		b = &localAuditBudget{tokens: localAuditBurst, last: now, seen: map[string]bool{}}
		l.workers[workerID] = b
	}
	if done, ok := b.seen[reportID]; ok {
		if done {
			return admitDuplicate
		}
		return admitInProgress
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
	b.seen[reportID] = false
	b.order = append(b.order, reportID)
	for len(b.order) > localAuditSeen {
		// Only a settled id leaves; one still being recorded stays reserved.
		if !b.seen[b.order[0]] {
			break
		}
		delete(b.seen, b.order[0])
		b.order = b.order[1:]
	}
	return admitOK
}

// settle ends the reservation of an admitted report: a recorded report stays
// known so that a repeated delivery is acknowledged; a failed one is
// forgotten so that a repeat records it.
func (l *LocalAudit) settle(workerID, reportID string, recorded bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.workers[workerID]
	if b == nil {
		return
	}
	if recorded {
		if _, ok := b.seen[reportID]; ok {
			b.seen[reportID] = true
		}
		return
	}
	delete(b.seen, reportID)
	if i := slices.Index(b.order, reportID); i >= 0 {
		b.order = slices.Delete(b.order, i, i+1)
	}
}

// sweep forgets, at most once a minute, the Workers that reported nothing
// for [localAuditIdle] and have no report being recorded. l.mu is held.
func (l *LocalAudit) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for id, b := range l.workers {
		if now.Sub(b.last) < localAuditIdle {
			continue
		}
		busy := false
		for _, done := range b.seen {
			if !done {
				busy = true
				break
			}
		}
		if !busy {
			delete(l.workers, id)
		}
	}
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
	if !ops.ValidUUIDv7(a.ReportID) {
		return audit.Event{}, errors.New("report_id must be a UUIDv7")
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
