package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// WorkerHiddenSessionsStore is the port of worker_hidden_sessions_set;
// *store.Store implements it.
type WorkerHiddenSessionsStore interface {
	WorkerOwner(ctx context.Context, id string) (string, error)
	SetWorkerHiddenSessionsIfOwner(ctx context.Context, workerID string, allow *bool, by, expectedOwnerID string) error
}

var _ WorkerHiddenSessionsStore = (*store.Store)(nil)

// RegisterWorkerHiddenSessions registers worker_hidden_sessions_set (v0.23.0)
// in r. hubPolicy returns the Hub setting hidden_sessions as it applies now.
//
// The dispatcher has decided operate on the Worker named by worker_id (the
// owner and administrators hold it; an API key also needs a scope on that
// Worker) and audits the call. The handler asks more: the caller is the
// Worker's owner, or holds manage on the Worker (an administrator, including
// on private and infrastructure Workers of other users). Someone who holds
// operate only through a grant, and a Worker identity, are refused: the
// choice applies to every caller of the Worker.
func RegisterWorkerHiddenSessions(r *Registry, st WorkerHiddenSessionsStore, dec Decider, hubPolicy func() string) error {
	if st == nil || dec == nil || hubPolicy == nil {
		return errors.New("api: worker_hidden_sessions_set needs a store, a decider and the hidden_sessions policy")
	}
	h := workerHiddenHandlers{st: st, dec: dec, hub: hubPolicy}
	if err := HandleTyped(r, ops.OpWorkerHiddenSessionsSet, h.set); err != nil {
		return fmt.Errorf("api: register worker hidden sessions: %w", err)
	}
	return nil
}

type workerHiddenHandlers struct {
	st  WorkerHiddenSessionsStore
	dec Decider
	hub func() string
}

var errWorkerHiddenNotAllowed = NewError(proto.CodeForbidden,
	"only the owner of this Worker or an administrator can change its hidden-sessions choice")

// allowed refuses a caller that is neither the Worker's owner nor holds
// manage on it. A caller allowed as the owner gets the owner back, so that
// the write re-checks it in its transaction; a caller allowed by manage gets
// "".
func (h workerHiddenHandlers) allowed(ctx context.Context, p authz.Principal, workerID string) (string, error) {
	if p.UserID == "" || p.Kind == authz.KindWorker {
		return "", errWorkerHiddenNotAllowed
	}
	owner, err := h.st.WorkerOwner(ctx, workerID)
	switch {
	case errors.Is(err, store.ErrWorkerNotFound):
		return "", NewError(proto.CodeNotFound, "the Worker does not exist")
	case err != nil:
		return "", err
	case owner == p.UserID:
		return owner, nil
	}
	err = h.dec.Decide(ctx, p, ops.MustActionSet(ops.ActionManage), authz.WorkerResource{ID: workerID})
	if err == nil {
		return "", nil
	}
	if e := ErrorOf(err); e.Code != proto.CodeForbidden && e.Code != proto.CodeNotFound {
		return "", err
	}
	return "", errWorkerHiddenNotAllowed
}

func (h workerHiddenHandlers) set(ctx context.Context, c *Call, a ops.WorkerHiddenSessionsSetArgs) (ops.WriteResult, error) {
	if c.Identity.Actor == "" {
		return ops.WriteResult{}, errWorkerHiddenNotAllowed
	}
	owner, err := h.allowed(ctx, c.Principal, a.WorkerID)
	if err != nil {
		return ops.WriteResult{}, err
	}
	// The audit row (written by the dispatcher on the Worker) carries the
	// new choice.
	c.AuditDetail = "hidden_sessions=" + a.HiddenSessions
	// An owner's choice is stored only while that user still owns the
	// Worker: a reassignment between the check and the write refuses it.
	if err := h.st.SetWorkerHiddenSessionsIfOwner(ctx, a.WorkerID, a.Allow(), c.Identity.Actor, owner); err != nil {
		switch {
		case errors.Is(err, store.ErrWorkerNotFound):
			return ops.WriteResult{}, NewError(proto.CodeNotFound, "the Worker does not exist")
		case errors.Is(err, store.ErrWorkerOwnerChanged):
			return ops.WriteResult{}, NewError(proto.CodeConflict, "the Worker changed owner; try again")
		}
		return ops.WriteResult{}, err
	}
	res := delivered()
	if hub := h.hub(); hub != config.HiddenSessionsUserControlled {
		res.Warnings = []ops.Warning{{Code: WarnHubPolicy,
			Message: "saved, but the Hub setting hidden_sessions is " + hub + ", so this Worker's choice applies only while it is user-controlled"}}
	}
	return res, nil
}
