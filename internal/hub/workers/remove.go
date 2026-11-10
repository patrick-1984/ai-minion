package workers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Audit vocabulary of the removal (AD-20). The target is the Worker id; no
// event holds a key, a name or any content.
const (
	AuditActionWorkerRemove = "worker.remove"
	AuditOutcomeOK          = "ok"
	AuditOutcomeFailed      = "failed"
)

// DefaultRevokeGrace is how long the Hub waits for an online Worker to
// confirm a live revoke before it ends the connection anyway.
const DefaultRevokeGrace = 2 * time.Second

// RemoveStore is the persistence port of the removal; *store.Store
// implements it.
type RemoveStore interface {
	WorkerByID(ctx context.Context, id string) (store.WorkerRow, error)
	// Tx runs fn in one transaction (AD-16).
	Tx(ctx context.Context, fn func(tx *sql.Tx) error) error
	// TombstoneWorkerTx replaces the Worker row by a tombstone inside tx.
	TombstoneWorkerTx(ctx context.Context, tx *sql.Tx, id, removedBy string, removedAt int64) error
}

var _ RemoveStore = (*store.Store)(nil)

// Cleanup is a step of the removal transaction that another domain owns: the
// trust rows (authz) and the API keys bound to the Worker (apikey). The
// workers package holds no SQL for them (AD-16); the step runs on the
// transaction of the removal, so that either everything is removed or
// nothing is.
type Cleanup interface {
	// RemoveWorkerTx deletes what the domain holds for the Worker: every
	// trust row for the Worker and its sessions, or every API key bound to it
	// (those keys are revoked, never left usable).
	RemoveWorkerTx(ctx context.Context, tx *sql.Tx, workerID string) error
}

// CleanupFunc adapts a function to [Cleanup].
type CleanupFunc func(ctx context.Context, tx *sql.Tx, workerID string) error

// RemoveWorkerTx implements [Cleanup].
func (f CleanupFunc) RemoveWorkerTx(ctx context.Context, tx *sql.Tx, workerID string) error {
	return f(ctx, tx, workerID)
}

// RemoveAuditor is the audit port of the removal; *audit.Log implements it.
// The ok record is written inside the removal transaction (RecordTx), so that
// no removal exists without its record and no ok record exists for a removal
// that did not happen (AD-20).
type RemoveAuditor interface {
	Auditor
	RecordTx(ctx context.Context, tx store.Execer, ev audit.Event) error
}

var _ RemoveAuditor = (*audit.Log)(nil)

// LiveRevoker delivers the signed revoke to an online Worker (AD-9).
// [RegistryRevoker] is the production implementation.
type LiveRevoker interface {
	// RevokeOnline sends the revoke to the Worker if it is connected and ends
	// its connection. online is false when the Worker was not connected;
	// confirmed is true when the Worker acknowledged the revoke before the
	// connection was ended.
	RevokeOnline(ctx context.Context, workerID string) (online, confirmed bool, err error)
}

// RemoveConfig configures a [Remover]. Store is required.
type RemoveConfig struct {
	Store RemoveStore
	// Trust and Keys run inside the removal transaction. A Hub that has not
	// wired a domain yet leaves it nil; the module that owns the tables
	// provides it (Stories 6.x).
	Trust Cleanup
	Keys  Cleanup
	// Live delivers the revoke to an online Worker; nil means no live
	// delivery: the Worker is revoked when it next connects.
	Live LiveRevoker
	// Auditor records the removal (AD-20): the ok record inside the removal
	// transaction, a failed removal outside it. Required.
	Auditor RemoveAuditor
	// Now defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Remover removes Workers (AD-9, AD-16, AD-17). Safe for concurrent use.
type Remover struct {
	cfg RemoveConfig
	log *slog.Logger
}

// NewRemover validates cfg and returns the remover.
func NewRemover(cfg RemoveConfig) (*Remover, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("workers: the remover needs a store")
	case cfg.Auditor == nil:
		return nil, errors.New("workers: the remover needs an audit log")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Remover{cfg: cfg, log: log}, nil
}

// RemoveActor says who removes the Worker, for the audit record.
type RemoveActor struct {
	// ID is the acting account or API key actor. Required.
	ID string
	// ActingAs is the user an admin acts as; empty otherwise.
	ActingAs string
}

// RemoveResult describes what a removal did.
type RemoveResult struct {
	WorkerID string
	Name     string
	// Online is true when the Worker was connected at the time.
	Online bool
	// Revoked is true when the connected Worker confirmed the signed revoke.
	// It is false for an offline Worker (the revoke is sent on its next
	// connect) and when the confirmation did not arrive in time (the Worker
	// then gets the revoke when it reconnects).
	Revoked bool
}

// Remove removes a Worker.
//
// In one transaction it deletes the Worker's trust rows and bound API keys
// and replaces the Worker row by a tombstone that keeps the public key. After
// the commit it sends the signed revoke to the Worker if it is online. A
// Worker that is offline, or that did not take the live revoke, is answered
// with the same signed revoke when it next connects: the connect endpoint
// checks the tombstone (AD-9). The removal is therefore complete once the
// transaction committed; the live delivery only makes it immediate.
//
// An unknown or already removed Worker is not_found. The ok audit record is
// written in the removal transaction: a record that cannot be written rolls
// the removal back, and a failed removal leaves exactly one record, with
// outcome "failed".
func (r *Remover) Remove(ctx context.Context, actor RemoveActor, workerID string) (RemoveResult, error) {
	if actor.ID == "" {
		return RemoveResult{}, errors.New("workers: removal without an actor")
	}
	w, err := r.cfg.Store.WorkerByID(ctx, workerID)
	if err != nil {
		return RemoveResult{}, notFound(err)
	}
	removedAt := store.UnixMS(r.cfg.Now())
	err = r.cfg.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, c := range []Cleanup{r.cfg.Trust, r.cfg.Keys} {
			if c == nil {
				continue
			}
			if err := c.RemoveWorkerTx(ctx, tx, workerID); err != nil {
				return err
			}
		}
		if err := r.cfg.Store.TombstoneWorkerTx(ctx, tx, workerID, actor.ID, removedAt); err != nil {
			return err
		}
		if err := r.cfg.Auditor.RecordTx(ctx, tx, r.event(actor, workerID, AuditOutcomeOK)); err != nil {
			return fmt.Errorf("audit record: %w", err)
		}
		return nil
	})
	if err != nil {
		_ = r.cfg.Auditor.Record(context.WithoutCancel(ctx), r.event(actor, workerID, AuditOutcomeFailed))
		if errors.Is(err, store.ErrWorkerNotFound) {
			return RemoveResult{}, notFound(err) // removed by someone else meanwhile
		}
		return RemoveResult{}, fmt.Errorf("workers: Worker %s was not removed: %w", workerID, err)
	}
	res := RemoveResult{WorkerID: w.ID, Name: w.Name}
	r.log.Info("worker removed", "worker_id", w.ID)

	if r.cfg.Live != nil {
		// The removal is committed: a failure here never undoes it.
		online, confirmed, err := r.cfg.Live.RevokeOnline(context.WithoutCancel(ctx), workerID)
		res.Online, res.Revoked = online, confirmed
		if err != nil {
			r.log.Warn("worker removed; the live revoke was not delivered, the Worker gets it when it reconnects",
				"worker_id", w.ID, "error", err.Error())
		}
	}
	return res, nil
}

// Warnings returns the warnings worker_remove reports for res.
func (res RemoveResult) Warnings() []ops.Warning {
	switch {
	case !res.Online:
		return []ops.Warning{{Code: ops.WarnRevokeOnNextConnect,
			Message: "the Worker is offline; it wipes its pairing the next time it connects"}}
	case !res.Revoked:
		return []ops.Warning{{Code: ops.WarnRevokeNotConfirmed,
			Message: "the Worker did not confirm the revoke in time; it wipes its pairing when it reconnects"}}
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, store.ErrWorkerNotFound) {
		return proto.NewError(proto.CodeNotFound, "no such Worker")
	}
	return err
}

func (r *Remover) event(a RemoveActor, workerID, outcome string) audit.Event {
	return audit.Event{
		Actor: a.ID, ActingAs: a.ActingAs, Action: AuditActionWorkerRemove, Target: workerID, Outcome: outcome,
	}
}

// RegistryRevoker is the production [LiveRevoker]: it sends the signed revoke
// over the live connection of the Worker in the [Registry].
//
// The revoke travels as the only frame of an op stream that the Hub opens
// (AD-3): {"type":"revoke","sig":...}, signed with purpose "revoke" over the
// transcript hash of this connection (AD-9). The Worker verifies it against
// its pinned Hub key, wipes its pairing and answers with an op.result frame
// ({"type":"op.result","op":"revoke"}); anything else it sends is not a
// confirmation. The Hub ends the connection after the answer or after Grace,
// whichever comes first. A Worker that did not take the frame therefore
// reconnects and is answered with the revoke of its tombstone.
type RegistryRevoker struct {
	Registry *Registry
	Identity *Identity
	// Grace bounds the wait for the Worker's answer; zero means
	// [DefaultRevokeGrace].
	Grace time.Duration
}

// RevokeOnline implements [LiveRevoker].
func (v *RegistryRevoker) RevokeOnline(ctx context.Context, workerID string) (online, confirmed bool, err error) {
	on, ok := v.Registry.Get(workerID)
	if !ok {
		return false, false, nil
	}
	// Whatever happens next, the connection of a removed Worker ends.
	defer v.Registry.Disconnect(workerID)

	frame, err := SignRevoke(v.Identity.Key(), on.TranscriptHash)
	if err != nil {
		return true, false, fmt.Errorf("sign the revoke: %w", err)
	}
	grace := v.Grace
	if grace <= 0 {
		grace = DefaultRevokeGrace
	}
	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	st, err := v.Registry.OpenControl(ctx, workerID)
	if err != nil {
		if proto.CodeOf(err) == proto.CodeWorkerOffline {
			return false, false, nil // it went offline meanwhile
		}
		return true, false, err
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(grace))
	if err := st.WriteFrame(frame); err != nil {
		return true, false, fmt.Errorf("send the revoke: %w", err)
	}
	body, err := st.ReadFrame()
	if err != nil {
		return true, false, fmt.Errorf("wait for the Worker's answer: %w", err)
	}
	var ack struct {
		Type string `json:"type"`
		Op   string `json:"op"`
	}
	if err := json.Unmarshal(body, &ack); err != nil || ack.Type != proto.TypeOpResult || ack.Op != "revoke" {
		return true, false, errors.New("the Worker did not accept the revoke")
	}
	return true, true, nil
}
