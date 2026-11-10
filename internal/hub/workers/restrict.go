package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// PolicyStore is the part of the workers table the Hub-side restriction needs;
// *store.Store implements it.
type PolicyStore interface {
	SetWorkerPreviewOnly(ctx context.Context, workerID string, on bool) (changed bool, err error)
	WorkerPreviewOnly(ctx context.Context, workerID string) (bool, error)
}

var _ PolicyStore = (*store.Store)(nil)

// RestrictionsConfig configures [NewRestrictions]. Store is required.
type RestrictionsConfig struct {
	Store PolicyStore
	// Connections ends the connection of an online Worker whose policy
	// changed, so that its reconnect reads the new policy; nil leaves it
	// connected until it reconnects by itself.
	Connections Disconnector
	// Auditor records a policy change; nil disables it.
	Auditor Auditor
	Logger  *slog.Logger
}

// Restrictions is the Hub-side restriction of Workers (AD-12, R97). The Hub
// stores a policy per Worker and delivers it in welcome. The Worker applies it
// on top of its own: the Hub can only add a restriction, never relax the
// Worker's local one, because the Worker enforces its local policy whatever
// the Hub sends.
type Restrictions struct {
	st    PolicyStore
	conns Disconnector
	audit Auditor
	log   *slog.Logger
}

// NewRestrictions validates cfg and returns the service.
func NewRestrictions(cfg RestrictionsConfig) (*Restrictions, error) {
	if cfg.Store == nil {
		return nil, errors.New("workers: the restriction service needs a store")
	}
	r := &Restrictions{st: cfg.Store, conns: cfg.Connections, audit: cfg.Auditor, log: cfg.Logger}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// Policy returns the policy to deliver in the welcome of w. It reads the
// table on every connect. It is the Policy hook of [Config] and
// [ServiceOptions] (see [Service.UsePolicy]).
func (r *Restrictions) Policy(ctx context.Context, w store.WorkerRow) (Policy, error) {
	on, err := r.st.WorkerPreviewOnly(ctx, w.ID)
	if err != nil {
		return Policy{}, fmt.Errorf("workers: read policy: %w", err)
	}
	return Policy{PreviewOnly: on}, nil
}

// RestrictResult tells what a policy change did.
type RestrictResult struct {
	// Changed is false when the Worker already had the requested policy.
	Changed bool
	// Disconnected reports that the Worker was online and its connection was
	// closed so that the reconnect re-reads the policy.
	Disconnected bool
}

// Restrict sets the Hub-side preview_only policy of a Worker. A change on an
// online Worker closes its connection: the Worker reconnects, and its welcome
// carries the new policy. A call that changes nothing leaves the connection
// alone. It returns not_found for an unknown Worker.
func (r *Restrictions) Restrict(ctx context.Context, actor Actor, workerID string, previewOnly bool) (RestrictResult, error) {
	if err := actor.validate(); err != nil {
		return RestrictResult{}, err
	}
	changed, err := r.st.SetWorkerPreviewOnly(ctx, workerID, previewOnly)
	switch {
	case errors.Is(err, store.ErrWorkerNotFound):
		return RestrictResult{}, proto.NewError(proto.CodeNotFound, "not found")
	case err != nil:
		return RestrictResult{}, fmt.Errorf("workers: restrict: %w", err)
	}
	res := RestrictResult{Changed: changed}
	if !changed {
		return res, nil
	}
	if r.audit != nil {
		ev := audit.Event{
			Actor: actor.ID, ActingAs: actor.ActingAs, Action: AuditWorkerRestrict,
			Target: fmt.Sprintf("%s preview_only=%t", workerID, previewOnly), Outcome: auditSuccess,
		}
		if err := r.audit.Record(context.WithoutCancel(ctx), ev); err != nil {
			r.log.Error("worker restriction audit write failed", "worker_id", workerID, "error", err.Error())
		}
	}
	res.Disconnected = r.conns != nil && r.conns.Disconnect(workerID)
	r.log.Info("worker policy changed", "worker_id", workerID, "preview_only", previewOnly, "disconnected", res.Disconnected)
	return res, nil
}

// UsePolicy makes the Hub deliver f's policy in the welcome of every Worker
// that connects from now on (for example [Restrictions.Policy]). Call it once
// while the Hub is being composed, before it serves connections: the connect
// handler reads the hook without a lock.
func (s *Service) UsePolicy(f func(ctx context.Context, w store.WorkerRow) (Policy, error)) {
	s.handler.cfg.Policy = f
}
