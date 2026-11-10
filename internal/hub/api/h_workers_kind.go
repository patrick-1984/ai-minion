package api

import (
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// WorkerKindService is what worker_reassign and worker_rename need from the
// Hub; *workers.Reassigner implements it.
type WorkerKindService interface {
	Reassign(ctx context.Context, actor workers.Actor, workerID, userID string) (workers.ReassignResult, error)
	Rename(ctx context.Context, actor workers.Actor, workerID, name string) error
}

// WorkerRestrictService is what worker_restrict needs from the Hub;
// *workers.Restrictions implements it.
type WorkerRestrictService interface {
	Restrict(ctx context.Context, actor workers.Actor, workerID string, previewOnly bool) (workers.RestrictResult, error)
}

var (
	_ WorkerKindService     = (*workers.Reassigner)(nil)
	_ WorkerRestrictService = (*workers.Restrictions)(nil)
)

// RegisterWorkerKindHandlers registers worker_reassign, worker_rename and
// worker_restrict (Story 6.6, AD-17, R97) on r.
//
// The dispatcher has already decided each call on the Worker named by
// worker_id: worker_reassign needs manage, which only admins hold, and the
// other two need operate, which the owner and admins hold (an API key also
// needs a scope on that Worker). The services repeat the checks that depend on
// stored state (a private Worker is never reassigned, names are unique per
// owner).
func RegisterWorkerKindHandlers(r *Registry, kinds WorkerKindService, restrict WorkerRestrictService) error {
	if kinds == nil || restrict == nil {
		return errors.New("api: the Worker administration handlers need both services")
	}
	h := workerKindHandlers{kinds: kinds, restrict: restrict}
	if err := HandleTyped(r, ops.OpWorkerReassign, h.reassign); err != nil {
		return err
	}
	if err := HandleTyped(r, ops.OpWorkerRename, h.rename); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpWorkerRestrict, h.restrictWorker)
}

type workerKindHandlers struct {
	kinds    WorkerKindService
	restrict WorkerRestrictService
}

// kindActor is the audit identity of the call.
func kindActor(c *Call) (workers.Actor, error) {
	if c.Identity.Actor == "" {
		return workers.Actor{}, NewError(proto.CodeForbidden, "not allowed")
	}
	return workers.Actor{ID: c.Identity.Actor, ActingAs: c.Identity.ActingAs}, nil
}

func (h workerKindHandlers) reassign(ctx context.Context, c *Call, a ops.WorkerReassignArgs) (ops.WriteResult, error) {
	who, err := kindActor(c)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if _, err := h.kinds.Reassign(ctx, who, a.WorkerID, a.UserID); err != nil {
		return ops.WriteResult{}, err
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
}

func (h workerKindHandlers) rename(ctx context.Context, c *Call, a ops.WorkerRenameArgs) (ops.WriteResult, error) {
	who, err := kindActor(c)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if err := h.kinds.Rename(ctx, who, a.WorkerID, a.Name); err != nil {
		return ops.WriteResult{}, err
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
}

func (h workerKindHandlers) restrictWorker(ctx context.Context, c *Call, a ops.WorkerRestrictArgs) (ops.WriteResult, error) {
	who, err := kindActor(c)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if _, err := h.restrict.Restrict(ctx, who, a.WorkerID, a.PreviewOnly); err != nil {
		return ops.WriteResult{}, err
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
}
