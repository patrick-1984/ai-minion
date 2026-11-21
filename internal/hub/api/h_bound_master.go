package api

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// RegisterBoundMaster uses the same manage action and pinned Worker resource
// as the name-only control mutation. The bound service separately verifies
// the live prepared incarnation before changing a nomination.
func RegisterBoundMaster(r *Registry, st *store.Store, change func(context.Context, store.WorkerRow, ops.ControlSetBoundMasterArgs, string) (bool, error)) error {
	pins := newOwnerPins[ops.ControlSetBoundMasterArgs]()
	resource := func(_ authz.Principal, args any) (authz.Resource, error) {
		a, ok := args.(*ops.ControlSetBoundMasterArgs)
		if !ok || a == nil {
			return nil, NewError(proto.CodeInvalidArgument, "master nomination arguments are required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), trustCaptureTimeout)
		defer cancel()
		w, err := st.WorkerByID(ctx, a.WorkerID)
		if err != nil {
			return nil, ownerErr(err)
		}
		pins.put(a, w.OwnerID+"\x00"+hex.EncodeToString(w.Pub))
		return authz.WorkerResource{ID: a.WorkerID}, nil
	}
	return HandleTyped(r, ops.OpControlSetBoundMaster, func(ctx context.Context, c *Call, a ops.ControlSetBoundMasterArgs) (ops.WriteResult, error) {
		p, _ := c.Args.(*ops.ControlSetBoundMasterArgs)
		fields, err := pinned(pins.take(p))
		if err != nil || len(fields) != 2 {
			return ops.WriteResult{}, NewError(proto.CodeNotFound, "no such Worker")
		}
		w, err := st.WorkerByID(ctx, a.WorkerID)
		if err != nil {
			return ops.WriteResult{}, ownerErr(err)
		}
		if w.OwnerID != fields[0] || hex.EncodeToString(w.Pub) != fields[1] {
			return ops.WriteResult{}, ownerErr(store.ErrWorkerOwnerChanged)
		}
		if _, err := change(ctx, w, a, c.Identity.Actor); err != nil {
			if errors.Is(err, store.ErrMasterCandidate) || errors.Is(err, store.ErrMasterRevision) {
				return ops.WriteResult{}, NewError(proto.CodeConflict, err.Error())
			}
			return ops.WriteResult{}, ownerErr(err)
		}
		return delivered(), nil
	}, WithResource(resource))
}
