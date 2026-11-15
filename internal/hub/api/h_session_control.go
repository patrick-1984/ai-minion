package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// SessionControlStore is what the session-control handlers need;
// *store.Store implements it.
type SessionControlStore interface {
	ListSessionControl(ctx context.Context) ([]store.SessionControlRow, error)
	WorkerOwner(ctx context.Context, workerID string) (string, error)
	PutSessionControlIfOwner(ctx context.Context, r store.SessionControlRow, expectedOwnerID string) error
	DeleteSessionControl(ctx context.Context, workerID, sessionName string) (bool, error)
}

var _ SessionControlStore = (*store.Store)(nil)

// RegisterSessionControl registers session_control_list and
// session_control_set on r. Both need manage, which only admins hold:
// session_control_list is decided on the caller's own user resource,
// session_control_set on the controller session it names, so an unknown
// Worker answers not_found. Grants are Hub-wide, so an API key whose scope
// does not reach the whole Hub is refused. The dispatcher writes the audit
// record of session_control_set, with the controller session as its target.
//
// Like trust_set, session_control_set captures the owner of the controller
// Worker before the dispatcher's decision and stores the grant only while the
// Worker still exists with that owner: a removal or reassignment that lands
// in between deletes the Worker's grants, and the in-flight request must not
// bring one back (it answers conflict, or not_found for a removed Worker).
func RegisterSessionControl(r *Registry, st SessionControlStore) error {
	if st == nil {
		return errors.New("api: the session-control handlers need a store")
	}
	op, ok := r.catalog.Lookup(ops.OpSessionControlSet)
	if !ok {
		return errors.New("api: session_control_set is not an operation of the catalog")
	}
	h := sessionControlHandlers{st: st, op: op, pins: newOwnerPins[ops.SessionControlSetArgs]()}
	if err := HandleTyped(r, ops.OpSessionControlList, h.list); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpSessionControlSet, h.set, WithResource(h.resource))
}

type sessionControlHandlers struct {
	st SessionControlStore
	op ops.Op
	// pins carries the owner captured by resource to the handler of the same
	// request.
	pins *ownerPins[ops.SessionControlSetArgs]
}

// resource derives the resource as the default does and captures the owner of
// the controller Worker first, before the dispatcher's decision. A Worker that
// does not exist is left uncaptured: the decision answers not_found.
func (h sessionControlHandlers) resource(p authz.Principal, args any) (authz.Resource, error) {
	res, err := DefaultResource(p, h.op, args)
	if err != nil {
		return nil, err
	}
	a, ok := args.(*ops.SessionControlSetArgs)
	if !ok || len(a.Targets) == 0 {
		return res, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustCaptureTimeout)
	defer cancel()
	owner, err := h.st.WorkerOwner(ctx, a.WorkerID)
	switch {
	case errors.Is(err, store.ErrWorkerNotFound):
		// Left unpinned: the decision answers not_found.
	case err != nil:
		return nil, err
	default:
		h.pins.put(a, owner)
	}
	return res, nil
}

func (h sessionControlHandlers) list(ctx context.Context, c *Call, _ ops.SessionControlListArgs) (ops.SessionControlListResult, error) {
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.SessionControlListResult{}, err
	}
	rows, err := h.st.ListSessionControl(ctx)
	if err != nil {
		return ops.SessionControlListResult{}, err
	}
	out := ops.SessionControlListResult{Grants: make([]ops.SessionControlInfo, 0, len(rows))}
	for _, row := range rows {
		var targets []ops.KeyResource
		if err := json.Unmarshal([]byte(row.TargetsJSON), &targets); err != nil {
			return ops.SessionControlListResult{}, fmt.Errorf("api: session control of %s:%s: %w", row.WorkerID, row.SessionName, err)
		}
		out.Grants = append(out.Grants, ops.SessionControlInfo{
			WorkerID: row.WorkerID, SessionName: row.SessionName, Targets: targets,
			CreatedBy: row.CreatedBy, CreatedAt: ops.Timestamp(store.FromUnixMS(row.CreatedAt)),
		})
	}
	return out, nil
}

func (h sessionControlHandlers) set(ctx context.Context, c *Call, a ops.SessionControlSetArgs) (ops.WriteResult, error) {
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.WriteResult{}, err
	}
	if len(a.Targets) == 0 {
		if _, err := h.st.DeleteSessionControl(ctx, a.WorkerID, a.SessionName); err != nil {
			return ops.WriteResult{}, err
		}
		return delivered(), nil
	}
	targets, err := json.Marshal(a.Targets)
	if err != nil {
		return ops.WriteResult{}, err
	}
	owner, pinned := "", false
	if args, ok := c.Args.(*ops.SessionControlSetArgs); ok {
		owner, pinned = h.pins.take(args)
	}
	if !pinned {
		// The Worker was unknown when the request began: fail closed.
		return ops.WriteResult{}, NewError(proto.CodeNotFound, "not found")
	}
	err = h.st.PutSessionControlIfOwner(ctx, store.SessionControlRow{
		WorkerID: a.WorkerID, SessionName: a.SessionName, TargetsJSON: string(targets),
		CreatedBy: c.Identity.Actor, CreatedAt: store.UnixMS(time.Now()),
	}, owner)
	switch {
	case errors.Is(err, store.ErrWorkerOwnerChanged):
		return ops.WriteResult{}, NewError(proto.CodeConflict, "the Worker changed owner; try again")
	case errors.Is(err, store.ErrWorkerNotFound):
		return ops.WriteResult{}, NewError(proto.CodeNotFound, "not found")
	case err != nil:
		return ops.WriteResult{}, err
	}
	return delivered(), nil
}
