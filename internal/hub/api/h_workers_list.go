package api

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// WorkerLister is what the workers_list handler needs; *workers.Router
// implements it. The principal's visible Workers are filtered by authz.Filter
// there: an admin sees their own Workers unless all is true.
type WorkerLister interface {
	ListWorkers(ctx context.Context, p authz.Principal, all bool) ([]ops.WorkerInfo, error)
}

// RegisterWorkersList registers workers_list in r. The operation is decided on
// the caller's own user resource (every signed-in user may list), and the
// result is filtered per Worker by the lister.
func RegisterWorkersList(r *Registry, l WorkerLister) error {
	return HandleTyped(r, ops.OpWorkersList, func(ctx context.Context, c *Call, args ops.WorkersListArgs) (ops.WorkersListResult, error) {
		list, err := l.ListWorkers(ctx, c.Principal, args.All)
		if err != nil {
			return ops.WorkersListResult{}, err
		}
		if list == nil {
			list = []ops.WorkerInfo{}
		}
		return ops.WorkersListResult{Workers: list}, nil
	})
}
