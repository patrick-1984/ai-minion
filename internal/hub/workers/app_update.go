package workers

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// WorkerAppUpdateSource reads how the Hub updates each Worker (v0.24.0,
// worker_app_update_set).
type WorkerAppUpdateSource interface {
	// WorkerAppUpdatePolicy is one Worker's policy: auto, off, or "" for
	// manual.
	WorkerAppUpdatePolicy(ctx context.Context, workerID string) (string, error)
	// WorkerAppUpdatePolicies is the policy of every Worker that has one.
	WorkerAppUpdatePolicies(ctx context.Context) (map[string]string, error)
}

// checkAppUpdate refuses worker_update for a Worker whose update policy on
// the Hub is off. A policy that cannot be read refuses too (fail closed).
func (rt *Router) checkAppUpdate(ctx context.Context, op, workerID string) error {
	if op != ops.OpWorkerUpdate || rt.appUpdate == nil {
		return nil
	}
	p, err := rt.appUpdate.WorkerAppUpdatePolicy(ctx, workerID)
	if err != nil {
		return proto.NewError(proto.CodeInternal, "the Hub cannot read the update policy of this Worker")
	}
	if p == ops.WorkerAppUpdateOff {
		return proto.NewError(proto.CodeForbidden, "updates of this Worker are turned off on the Hub (Updates from the Hub: Off on its Worker page)")
	}
	return nil
}
