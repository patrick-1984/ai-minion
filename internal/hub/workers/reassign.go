package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Audit vocabulary of Worker administration (AD-20). Targets are a Worker id
// followed by key=value pairs of identifiers (the kind, the new owner, the
// policy); no event holds a name that a user typed or any content.
const (
	AuditWorkerReassign = "worker.reassign"
	AuditWorkerRename   = "worker.rename"
	AuditWorkerRestrict = "worker.restrict"
)

// Actor is who an administration call is audited as. The handler fills it from
// the authenticated request.
type Actor struct {
	// ID is the audit actor: a user id, "key:<id>" for an API key.
	ID string
	// ActingAs is the user an admin acts as (act-as only).
	ActingAs string
}

func (a Actor) validate() error {
	if a.ID == "" {
		return proto.NewError(proto.CodeForbidden, "not allowed")
	}
	return nil
}

// Disconnector closes the connection of an online Worker; *Registry
// implements it. A Worker reconnects by itself, and the reconnect reads
// everything the Hub knows about it again (name, owner, policy).
type Disconnector interface {
	Disconnect(workerID string) bool
}

var _ Disconnector = (*Registry)(nil)

// ReassignStore is the part of the workers tables the reassignment service
// needs; *store.Store implements it.
type ReassignStore interface {
	WorkerByID(ctx context.Context, id string) (store.WorkerRow, error)
	ReassignWorker(ctx context.Context, p store.ReassignParams) (store.ReassignResult, error)
	RenameWorker(ctx context.Context, workerID, name string) (store.WorkerRow, error)
}

var _ ReassignStore = (*store.Store)(nil)

// ReassignConfig configures [NewReassigner]. Store is required.
type ReassignConfig struct {
	Store ReassignStore
	// Connections ends the connection of a Worker whose name or owner
	// changed, so that its next welcome carries the new values; nil leaves
	// online Workers untouched until they reconnect.
	Connections Disconnector
	// Auditor records refusals; nil disables it. The audit row of a success
	// is written in the transaction of the change itself.
	Auditor Auditor
	Logger  *slog.Logger
}

// Reassigner moves infrastructure Workers between users and renames Workers
// (AD-17, R97). Who may call it is decided before: the dispatcher requires
// manage for a reassignment (admins only) and operate for a rename.
type Reassigner struct {
	st    ReassignStore
	conns Disconnector
	audit Auditor
	log   *slog.Logger
	now   func() int64
}

// NewReassigner validates cfg and returns the service.
func NewReassigner(cfg ReassignConfig) (*Reassigner, error) {
	if cfg.Store == nil {
		return nil, errors.New("workers: the reassignment service needs a store")
	}
	r := &Reassigner{st: cfg.Store, conns: cfg.Connections, audit: cfg.Auditor, log: cfg.Logger, now: store.NowMS}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// ReassignResult tells what a reassignment changed.
type ReassignResult struct {
	// TrustDeleted is the number of trust rows deleted (every receiver's).
	TrustDeleted int64
	// KeysRevoked is the number of API keys bound to the Worker that were
	// revoked.
	KeysRevoked int64
	// Disconnected reports that the Worker was online and its connection was
	// closed.
	Disconnected bool
}

// Reassign makes userID the owner of an infrastructure Worker. In one
// transaction the owner changes, every trust row for the Worker and its
// sessions is deleted (the admin's included) and the API keys bound to the
// Worker are revoked; an online Worker is then disconnected so that it
// reconnects under its new owner. It returns
//
//   - not_found for an unknown Worker or user;
//   - forbidden for a private Worker, whoever asks (AD-17);
//   - conflict when the new owner already has a Worker of this name, or is
//     deactivated;
//   - invalid_argument when the Worker already belongs to the user.
func (r *Reassigner) Reassign(ctx context.Context, actor Actor, workerID, userID string) (ReassignResult, error) {
	if err := actor.validate(); err != nil {
		return ReassignResult{}, err
	}
	w, err := r.st.WorkerByID(ctx, workerID)
	if err != nil {
		return ReassignResult{}, r.refuse(ctx, actor, workerID, "", userID, err)
	}
	// The kind is checked inside the transaction as well; this read only
	// gives the refusal its audit row.
	if w.Kind != store.WorkerKindInfrastructure {
		return ReassignResult{}, r.refuse(ctx, actor, workerID, w.Kind, userID, store.ErrWorkerNotInfrastructure)
	}
	res, err := r.st.ReassignWorker(ctx, store.ReassignParams{
		WorkerID: workerID, NewOwnerID: userID, AtMS: r.now(),
		Audit: &store.AuditRow{
			ID: store.NewID(), TS: r.now(), Actor: actor.ID, ActingAs: actor.ActingAs,
			Action: AuditWorkerReassign, Target: reassignTarget(workerID, w.Kind, userID), Outcome: auditSuccess,
		},
	})
	if err != nil {
		return ReassignResult{}, r.refuse(ctx, actor, workerID, w.Kind, userID, err)
	}
	out := ReassignResult{TrustDeleted: res.TrustDeleted, KeysRevoked: res.KeysRevoked}
	out.Disconnected = r.disconnect(workerID)
	r.log.Info("worker reassigned", "worker_id", workerID, "trust_deleted", res.TrustDeleted, "keys_revoked", res.KeysRevoked)
	return out, nil
}

func reassignTarget(workerID, kind, userID string) string {
	return fmt.Sprintf("%s kind=%s to=%s", workerID, kind, userID)
}

// refuse maps a store error to the proto error of the API, audits the refusal
// under its code and returns the error.
func (r *Reassigner) refuse(ctx context.Context, actor Actor, workerID, kind, userID string, err error) error {
	var pe *proto.Error
	switch {
	case errors.Is(err, store.ErrWorkerNotFound):
		pe = proto.NewError(proto.CodeNotFound, "not found")
	case errors.Is(err, store.ErrWorkerNotInfrastructure):
		pe = proto.NewError(proto.CodeForbidden, "only an infrastructure Worker can be reassigned")
	case errors.Is(err, store.ErrNotFound):
		pe = proto.NewError(proto.CodeNotFound, "the new owner does not exist")
	case errors.Is(err, store.ErrUserInactive):
		pe = proto.NewError(proto.CodeConflict, "the new owner is deactivated")
	case errors.Is(err, store.ErrWorkerSameOwner):
		pe = proto.NewError(proto.CodeInvalidArgument, "the Worker already belongs to this user")
	case errors.Is(err, store.ErrWorkerNameTaken):
		pe = proto.NewError(proto.CodeConflict, "the new owner already has a Worker with this name")
	default:
		return fmt.Errorf("workers: reassign: %w", err)
	}
	if kind != "" {
		r.record(ctx, actor, AuditWorkerReassign, reassignTarget(workerID, kind, userID), string(pe.Code))
	}
	return pe
}

// Rename gives a Worker a new name, unique among its owner's Workers. An
// online Worker is disconnected so that its welcome carries the new name. It
// returns not_found for an unknown Worker and conflict when the owner already
// has a Worker of that name. Renaming to the current name succeeds and
// changes nothing.
func (r *Reassigner) Rename(ctx context.Context, actor Actor, workerID, name string) error {
	if err := actor.validate(); err != nil {
		return err
	}
	old, err := r.st.WorkerByID(ctx, workerID)
	if errors.Is(err, store.ErrWorkerNotFound) {
		return proto.NewError(proto.CodeNotFound, "not found")
	} else if err != nil {
		return fmt.Errorf("workers: rename: %w", err)
	}
	if old.Name == name {
		return nil
	}
	if _, err := r.st.RenameWorker(ctx, workerID, name); err != nil {
		switch {
		case errors.Is(err, store.ErrWorkerNotFound):
			return proto.NewError(proto.CodeNotFound, "not found")
		case errors.Is(err, store.ErrWorkerNameTaken):
			r.record(ctx, actor, AuditWorkerRename, workerID, string(proto.CodeConflict))
			return proto.NewError(proto.CodeConflict, "you already have a Worker with this name")
		}
		return fmt.Errorf("workers: rename: %w", err)
	}
	r.record(ctx, actor, AuditWorkerRename, workerID, auditSuccess)
	r.disconnect(workerID)
	return nil
}

func (r *Reassigner) disconnect(workerID string) bool {
	return r.conns != nil && r.conns.Disconnect(workerID)
}

func (r *Reassigner) record(ctx context.Context, a Actor, action, target, outcome string) {
	if r.audit == nil {
		return
	}
	ev := audit.Event{Actor: a.ID, ActingAs: a.ActingAs, Action: action, Target: target, Outcome: outcome}
	if err := r.audit.Record(context.WithoutCancel(ctx), ev); err != nil {
		r.log.Error("worker administration audit write failed", "action", action, "error", err.Error())
	}
}
