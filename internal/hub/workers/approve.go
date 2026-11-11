package workers

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// AuditWorkerApprove is the audit action of an approval (F-02). The row names
// the Worker id; the enrollment row ("worker.enroll", outcome pending) names
// the same id.
const AuditWorkerApprove = "worker.approve"

// ApproveStore is the part of the workers table the approval needs;
// *store.Store implements it.
type ApproveStore interface {
	WorkerByID(ctx context.Context, id string) (store.WorkerRow, error)
	ApproveWorker(ctx context.Context, id, by string, nowMS int64, a *store.AuditRow) (store.WorkerRow, error)
}

var _ ApproveStore = (*store.Store)(nil)

// ApproverConfig configures [NewApprover]. Store is required.
type ApproverConfig struct {
	Store ApproveStore
	// Connections ends the connection of an online Worker after its approval,
	// so that the reconnect is registered as an approved Worker (the same
	// mechanism as a policy change); nil leaves it connected.
	Connections Disconnector
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Approver approves pending Workers (F-02): enrollment is not authorization.
type Approver struct {
	st    ApproveStore
	conns Disconnector
	now   func() time.Time
	log   *slog.Logger
}

// NewApprover validates cfg and returns the service.
func NewApprover(cfg ApproverConfig) (*Approver, error) {
	if cfg.Store == nil {
		return nil, errors.New("workers: the approval service needs a store")
	}
	a := &Approver{st: cfg.Store, conns: cfg.Connections, now: cfg.Now, log: cfg.Logger}
	if a.now == nil {
		a.now = time.Now
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a, nil
}

// ApproveResult tells what an approval did.
type ApproveResult struct {
	// Changed is false when the Worker was approved already.
	Changed bool
	// Disconnected reports that the Worker was online and its connection was
	// closed so that it reconnects as an approved Worker.
	Disconnected bool
}

// Approve approves the pending Worker workerID. When fingerprint is not empty
// it must be the fingerprint of the Worker's key, otherwise nothing is
// approved (conflict). Approving an approved Worker succeeds without change. A
// first approval is audited in the same transaction.
func (a *Approver) Approve(ctx context.Context, actor Actor, workerID, fingerprint string) (ApproveResult, error) {
	if err := actor.validate(); err != nil {
		return ApproveResult{}, err
	}
	w, err := a.st.WorkerByID(ctx, workerID)
	switch {
	case errors.Is(err, store.ErrWorkerNotFound):
		return ApproveResult{}, proto.NewError(proto.CodeNotFound, "not found")
	case err != nil:
		return ApproveResult{}, fmt.Errorf("workers: approve: %w", err)
	}
	if fingerprint != "" {
		fp, _ := identity.FingerprintOf(ed25519.PublicKey(w.Pub))
		if fp.String() != fingerprint {
			return ApproveResult{}, proto.NewError(proto.CodeConflict,
				"the fingerprint does not match this Worker's key; nothing was approved; list the Workers again and compare")
		}
	}
	now := store.UnixMS(a.now())
	_, err = a.st.ApproveWorker(ctx, workerID, actor.ID, now, &store.AuditRow{
		ID: store.NewID(), TS: now, Actor: actor.ID, ActingAs: actor.ActingAs,
		Action: AuditWorkerApprove, Outcome: auditSuccess,
	})
	switch {
	case errors.Is(err, store.ErrWorkerAlreadyApproved):
		return ApproveResult{}, nil
	case errors.Is(err, store.ErrWorkerNotFound):
		return ApproveResult{}, proto.NewError(proto.CodeNotFound, "not found")
	case err != nil:
		return ApproveResult{}, fmt.Errorf("workers: approve: %w", err)
	}
	res := ApproveResult{Changed: true}
	res.Disconnected = a.conns != nil && a.conns.Disconnect(workerID)
	a.log.Info("worker approved", "worker_id", workerID, "by", actor.ID, "disconnected", res.Disconnected)
	return res, nil
}
