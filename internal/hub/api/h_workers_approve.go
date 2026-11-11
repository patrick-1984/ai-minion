package api

import (
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// WorkerApproveService is what worker_approve needs from the Hub;
// *workers.Approver implements it.
type WorkerApproveService interface {
	Approve(ctx context.Context, actor workers.Actor, workerID, fingerprint string) (workers.ApproveResult, error)
}

var _ WorkerApproveService = (*workers.Approver)(nil)

// RegisterWorkerApprove registers worker_approve (F-02) on r. The dispatcher
// has already decided the call on the Worker named by worker_id: the owner and
// admins hold operate on it, an admin's API key holds manage through its
// scope. A Worker identity holds neither. The handler adds one more refusal: an
// API key that is bound to the very Worker cannot approve it, so a credential
// that lives on a pending machine never approves that machine.
func RegisterWorkerApprove(r *Registry, svc WorkerApproveService) error {
	if svc == nil {
		return errors.New("api: worker_approve needs a service")
	}
	return HandleTyped(r, ops.OpWorkerApprove, func(ctx context.Context, c *Call, a ops.WorkerApproveArgs) (ops.WriteResult, error) {
		if c.Identity.Actor == "" {
			return ops.WriteResult{}, NewError(proto.CodeForbidden, "not allowed")
		}
		if p := c.Principal; p.Kind == authz.KindWorker || (p.Kind == authz.KindAPIKey && p.WorkerID == a.WorkerID) {
			return ops.WriteResult{}, NewError(proto.CodeForbidden, "a Worker cannot approve itself")
		}
		who := workers.Actor{ID: c.Identity.Actor, ActingAs: c.Identity.ActingAs}
		if _, err := svc.Approve(ctx, who, a.WorkerID, a.Fingerprint); err != nil {
			return ops.WriteResult{}, err
		}
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
	})
}
