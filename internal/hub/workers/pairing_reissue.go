package workers

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// RepairWindow is how long the owner's pairing re-issue stays open (design
// 12.3).
const RepairWindow = 10 * time.Minute

// RepairStore is the part of the database the pairing re-issue uses;
// *store.Store implements it.
type RepairStore interface {
	WorkerByID(ctx context.Context, id string) (store.WorkerRow, error)
	SetRepairWindow(ctx context.Context, workerID string, until int64) error
}

// ReissueRequest is one worker_pairing_reissue call.
type ReissueRequest struct {
	WorkerID string
	// UserID is the calling user (for an act-as call, the user acted as);
	// Admin says that the caller is an administrator.
	UserID string
	Admin  bool
	// Actor and ActingAs are the audit identity of the call.
	Actor    string
	ActingAs string
}

// ReissuePairing opens the 10 minute repair window of a Worker whose pairing
// the Hub committed but the Worker never recorded (design 12.3): allowed for
// the Worker's owner and for administrators, and only for a live Worker row
// that has never completed a connect handshake (a Worker removed, or one that
// existed before the upgrade, is not eligible: conflict). The eligibility
// check and the write are one conditional update. Nothing else changes; the
// owner then enters the new code the Worker shows.
func ReissuePairing(ctx context.Context, st RepairStore, a Auditor, now time.Time, req ReissueRequest) error {
	w, err := st.WorkerByID(ctx, req.WorkerID)
	switch {
	case errors.Is(err, store.ErrWorkerNotFound):
		return proto.NewError(proto.CodeNotFound, "no such Worker")
	case err != nil:
		return err
	case w.OwnerID != req.UserID && !req.Admin:
		// Do not reveal Workers of other owners.
		return proto.NewError(proto.CodeNotFound, "no such Worker")
	}
	err = st.SetRepairWindow(ctx, w.ID, store.UnixMS(now.Add(RepairWindow)))
	if errors.Is(err, store.ErrRepairNotAllowed) {
		return proto.NewError(proto.CodeConflict, "this Worker cannot be re-paired: it has connected before, or it was removed")
	}
	if err != nil {
		return err
	}
	if a != nil {
		actor := req.Actor
		if actor == "" {
			actor = req.UserID
		}
		_ = a.Record(context.WithoutCancel(ctx), audit.Event{
			Actor: actor, ActingAs: req.ActingAs, Action: AuditPairingReissue, Target: w.ID, Outcome: "window_opened",
		})
	}
	return nil
}
