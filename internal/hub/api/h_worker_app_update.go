package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// WorkerAppUpdateStore is the port of worker_app_update_set; *store.Store
// implements it.
type WorkerAppUpdateStore interface {
	SetWorkerAppUpdate(ctx context.Context, workerID, policy, by string) error
}

var _ WorkerAppUpdateStore = (*store.Store)(nil)

// RegisterWorkerAppUpdate registers worker_app_update_set (v0.24.0) in r.
//
// The dispatcher has decided manage on the Worker named by worker_id (an
// administrator) and audits the call with the new policy. The handler stores
// the user id of the administrator: an automatic update is sent later with
// that person's rights as they are then, so a former administrator's choice
// sends nothing. An act-as session and a Worker identity are refused.
func RegisterWorkerAppUpdate(r *Registry, st WorkerAppUpdateStore) error {
	if st == nil {
		return errors.New("api: worker_app_update_set needs a store")
	}
	h := workerAppUpdateHandlers{st: st}
	if err := HandleTyped(r, ops.OpWorkerAppUpdateSet, h.set); err != nil {
		return fmt.Errorf("api: register worker app update: %w", err)
	}
	return nil
}

type workerAppUpdateHandlers struct{ st WorkerAppUpdateStore }

var errWorkerAppUpdateNotAllowed = NewError(proto.CodeForbidden,
	"only an administrator, acting as themselves, can change how the Hub updates this Worker")

func (h workerAppUpdateHandlers) set(ctx context.Context, c *Call, a ops.WorkerAppUpdateSetArgs) (ops.WriteResult, error) {
	p := c.Principal
	if c.Identity.Actor == "" || p.UserID == "" || p.Kind == authz.KindWorker || p.ActorID != "" {
		return ops.WriteResult{}, errWorkerAppUpdateNotAllowed
	}
	c.AuditDetail = "app_update=" + a.Policy
	if err := h.st.SetWorkerAppUpdate(ctx, a.WorkerID, a.Policy, p.UserID); err != nil {
		if errors.Is(err, store.ErrWorkerNotFound) {
			return ops.WriteResult{}, NewError(proto.CodeNotFound, "the Worker does not exist")
		}
		return ops.WriteResult{}, err
	}
	return delivered(), nil
}
