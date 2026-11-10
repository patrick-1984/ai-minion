package api

import (
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// WorkerRemover removes a Worker (Story 4.10). *workers.Remover implements it.
type WorkerRemover interface {
	Remove(ctx context.Context, actor workers.RemoveActor, workerID string) (workers.RemoveResult, error)
}

var _ WorkerRemover = (*workers.Remover)(nil)

// WorkerRemoveHandler implements the operation worker_remove. The framework
// has already authenticated the caller and decided "operate" on the Worker
// through authz.Decide, so only the owner of the Worker and admins get here
// (AD-17, A-8); a foreign user sees not_found.
type WorkerRemoveHandler struct {
	Remover WorkerRemover
}

// Remove runs the removal as the call's actor. Every outcome is a delivered
// write: a removal that committed is complete whether or not the Worker was
// online, and the warnings say when the Worker learns about it later.
func (h *WorkerRemoveHandler) Remove(ctx context.Context, c *Call, args ops.WorkerRemoveArgs) (ops.WriteResult, error) {
	if h.Remover == nil {
		return ops.WriteResult{}, errors.New("api: worker_remove has no remover")
	}
	res, err := h.Remover.Remove(ctx, workers.RemoveActor{ID: c.Identity.Actor, ActingAs: c.Identity.ActingAs}, args.WorkerID)
	if err != nil {
		return ops.WriteResult{}, err
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered, Warnings: res.Warnings()}, nil
}

// RegisterWorkerRemove binds worker_remove in r. The Hub's api module calls it
// once the workers service exists.
func RegisterWorkerRemove(r *Registry, h *WorkerRemoveHandler) error {
	return HandleTyped(r, ops.OpWorkerRemove, h.Remove)
}
