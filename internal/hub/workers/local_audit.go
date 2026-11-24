package workers

import (
	"context"
	"encoding/json"
	"errors"
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

// LocalAudit serves proto.OpLocalAuditV1: it turns the report of a Worker
// about a local write on its own session into an audit event. The actor is
// the Worker's own local entry point ("worker:<id>/local", or
// "worker:<id>/session:<name>" when the calling terminal was a session of that
// Worker); the Worker is authenticated by its connection, never by the report.
type LocalAudit struct {
	auditor *audit.Log
}

// NewLocalAudit returns the service that records local-operation reports.
func NewLocalAudit(a *audit.Log) *LocalAudit { return &LocalAudit{auditor: a} }

// Bind registers the call op on service.
func (l *LocalAudit) Bind(service *Service) error {
	if !service.HandleCallOp(proto.OpLocalAuditV1, l.serve) {
		return errors.New("workers: the local audit report is already registered")
	}
	return nil
}

func (l *LocalAudit) serve(ctx context.Context, c Call) {
	var a proto.LocalAuditArgs
	if err := json.Unmarshal(c.Args, &a); err != nil {
		_ = c.Stream.WriteFrame(proto.NewError(proto.CodeInvalidArgument, "invalid local audit report").Frame())
		return
	}
	ev, err := localAuditEvent(c.Worker.ID, a)
	if err != nil {
		_ = c.Stream.WriteFrame(proto.NewError(proto.CodeInvalidArgument, err.Error()).Frame())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), localAuditTimeout)
	defer cancel()
	if err := l.auditor.Record(ctx, ev); err != nil {
		_ = c.Stream.WriteFrame(proto.NewError(proto.CodeInternal, "").Frame())
		return
	}
	_ = c.Stream.SetWriteDeadline(time.Now().Add(controlTimeout))
	_ = c.Stream.WriteFrame(OpResult{Type: proto.TypeOpResult, Result: json.RawMessage(`{}`)})
}

// localAuditEvent validates a report and builds its event. Only a state
// changing Worker operation of the catalog is accepted, so a report cannot
// fake another kind of event.
func localAuditEvent(workerID string, a proto.LocalAuditArgs) (audit.Event, error) {
	op, ok := ops.Lookup(a.Op)
	if !ok || op.Scope != ops.ScopeWorker || !op.ChangesState() {
		return audit.Event{}, errors.New("not a local write operation")
	}
	actor := "worker:" + workerID + "/local"
	if a.SourceSession != "" {
		actor = "worker:" + workerID + "/session:" + a.SourceSession
	}
	target := workerID
	if a.Session != "" {
		target += ":" + a.Session
	}
	outcome := a.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	if a.Reason != "" {
		outcome += ":" + a.Reason
	}
	ev := audit.Event{Actor: actor, Action: AuditLocalOp + a.Op, Target: target, Outcome: outcome}
	if err := ev.Validate(); err != nil {
		return audit.Event{}, err
	}
	return ev, nil
}
