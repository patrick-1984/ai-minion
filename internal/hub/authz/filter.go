package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Filter returns the items of a list result that p may read; the others are
// removed (AD-11: the Hub filters list results by scope). resource maps an
// item to the resource it stands for. An item whose resource no longer exists
// is dropped. The input slice is not modified. The principal and the owners are
// read from the store for this call only; a failing lookup returns an
// error and no items.
func Filter[T any](ctx context.Context, a *Authorizer, p Principal, items []T, resource func(T) Resource) ([]T, error) {
	out := make([]T, 0, len(items))
	pt := a.ports()
	pt.workers = newMemoWorkers(pt.workers)
	sub, ok, err := a.subject(ctx, p, pt)
	if err != nil || !ok {
		return out, err
	}
	read := ops.MustActionSet(ops.ActionRead)
	for _, it := range items {
		resolved, err := resource(it).resolve(ctx, pt)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("authz: resolve resource: %w", err)
		}
		if read.Permits(sub.actions(resolved)...) {
			out = append(out, it)
		}
	}
	return out, nil
}
