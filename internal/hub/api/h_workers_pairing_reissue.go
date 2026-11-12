package api

import (
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// PairingReissuer is what worker_pairing_reissue needs from the Hub: the
// pairing re-issue of design 12.3 (workers.ReissuePairing) and whether a
// user is an administrator.
type PairingReissuer interface {
	ReissuePairing(ctx context.Context, req workers.ReissueRequest) error
	IsAdmin(ctx context.Context, userID string) (bool, error)
}

// RegisterWorkerPairingReissue registers worker_pairing_reissue on r. The
// dispatcher has already required operate on the Worker named by worker_id
// (its owner and administrators hold it). The service checks owner or
// administrator again, together with the eligibility (a live Worker that has
// never connected), in one conditional update; an ineligible Worker is a
// conflict. A Worker identity never re-issues a pairing.
func RegisterWorkerPairingReissue(r *Registry, svc PairingReissuer) error {
	if svc == nil {
		return errors.New("api: worker_pairing_reissue needs a service")
	}
	return HandleTyped(r, ops.OpWorkerPairingReissue, func(ctx context.Context, c *Call, a ops.WorkerPairingReissueArgs) (ops.WriteResult, error) {
		p := c.Principal
		if c.Identity.Actor == "" || p.Kind == authz.KindWorker || p.UserID == "" {
			return ops.WriteResult{}, NewError(proto.CodeForbidden, "not allowed")
		}
		admin, err := svc.IsAdmin(ctx, p.UserID)
		if err != nil {
			return ops.WriteResult{}, err
		}
		err = svc.ReissuePairing(ctx, workers.ReissueRequest{
			WorkerID: a.WorkerID, UserID: p.UserID, Admin: admin,
			Actor: c.Identity.Actor, ActingAs: c.Identity.ActingAs,
		})
		if err != nil {
			return ops.WriteResult{}, err
		}
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
	})
}
