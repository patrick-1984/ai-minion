package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Filter returns the items of a list result that p may read; the others are
// removed (AD-11: the Hub filters list results by scope, the scope of an API
// key and the session-control grant of a Worker identity alike). resource maps an
// item to the resource it stands for. An item whose resource no longer exists
// is dropped. The input slice is not modified. The principal and the owners are
// read from the store for this call only; a failing lookup returns an
// error and no items.
func Filter[T any](ctx context.Context, a *Authorizer, p Principal, items []T, resource func(T) Resource) ([]T, error) {
	owned, err := FilterOwned(ctx, a, p, items, resource)
	if err != nil {
		return nil, err
	}
	out := make([]T, len(owned))
	for i, o := range owned {
		out[i] = o.Item
	}
	return out, nil
}

// Owned is an item [FilterOwned] kept, with the owner of its resource as it
// was read for the decision that kept it.
type Owned[T any] struct {
	Item    T
	OwnerID string
}

// FilterOwned is [Filter] that also returns, for each item kept, the owner
// its read was authorized under. A caller that shows data bound to the owner
// it was reported under compares that owner with this one snapshot.
func FilterOwned[T any](ctx context.Context, a *Authorizer, p Principal, items []T, resource func(T) Resource) ([]Owned[T], error) {
	out := make([]Owned[T], 0, len(items))
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
		h, err := held(ctx, sub, resolved, pt)
		if err != nil {
			return nil, err
		}
		if read.Permits(h...) {
			out = append(out, Owned[T]{Item: it, OwnerID: resolved.OwnerID})
		}
	}
	return out, nil
}
