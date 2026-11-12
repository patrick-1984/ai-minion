package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Worker kinds as stored (AD-17). The workers package owns their meaning.
const (
	WorkerKindPrivate        = "private"
	WorkerKindInfrastructure = "infrastructure"
)

// Errors of the workers tables. Callers match them with errors.Is.
var (
	// ErrWorkerNotFound: no workers-table row matches (distinct from the users
	// tables' not-found sentinel).
	ErrWorkerNotFound = errors.New("store: worker not found")
	// ErrWorkerExists: a Worker with this public key already exists.
	ErrWorkerExists = errors.New("store: a Worker with this public key already exists")
	// ErrWorkerNameTaken: the owner already has a Worker with this name.
	ErrWorkerNameTaken = errors.New("store: the owner already has a Worker with this name")
)

// HubIdentityRow is the single hub_identity row: the public key that
// <data>/identity.key must hold.
type HubIdentityRow struct {
	Pub         []byte
	Fingerprint string
	CreatedAt   int64 // unix milliseconds
}

// WorkerRow is one enrolled Worker.
type WorkerRow struct {
	ID        string // UUIDv7
	Pub       []byte // the Worker's raw Ed25519 public key
	Name      string
	OwnerID   string
	Kind      string // WorkerKindPrivate or WorkerKindInfrastructure
	CreatedAt int64  // unix milliseconds
	// Pending is true while the Worker waits for approval (F-02): it may
	// connect, but receives no operations and may not make calls. The zero
	// value is an approved Worker; enrollment sets it explicitly.
	Pending bool
	// EnrollAddr is the client address the Worker enrolled from, shown to
	// whoever approves it.
	EnrollAddr string
	// ApprovedAt (unix milliseconds, 0 while pending) and ApprovedBy (the
	// approving identity, or "migration") record the approval.
	ApprovedAt int64
	ApprovedBy string
	// FirstConnectedAt (unix milliseconds, 0 = never) is when a connect
	// handshake first registered the Worker; rows that existed at migration
	//  0023 carry their created_at. RepairUntil (0 = none) is the end of the
	// owner's pairing re-issue window (hub-identity-v2 12.3).
	FirstConnectedAt int64
	RepairUntil      int64
}

// TombstoneRow is the record a removed Worker leaves behind (AD-9).
type TombstoneRow struct {
	WorkerID  string
	Pub       []byte
	Name      string
	OwnerID   string
	Kind      string
	CreatedAt int64 // when the Worker was enrolled
	RemovedAt int64 // unix milliseconds
	RemovedBy string
}

// HubIdentity returns the recorded Hub identity, or ok=false when none has
// been recorded yet.
func (s *Store) HubIdentity(ctx context.Context) (row HubIdentityRow, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT pub, fingerprint, created_at FROM hub_identity WHERE id = 1`).
		Scan(&row.Pub, &row.Fingerprint, &row.CreatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return HubIdentityRow{}, false, nil
	case err != nil:
		return HubIdentityRow{}, false, fmt.Errorf("store: read hub identity: %w", err)
	}
	return row, true, nil
}

// PutHubIdentity records the Hub identity. It never replaces an existing
// record: the identity is permanent (AD-5), so a second call with any key
// fails.
func (s *Store) PutHubIdentity(ctx context.Context, row HubIdentityRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO hub_identity (id, pub, fingerprint, created_at) VALUES (1, ?, ?, ?)`,
		row.Pub, row.Fingerprint, row.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: record hub identity: %w", err)
	}
	return nil
}

// InsertWorker adds a Worker row through ex, which is the store itself or a
// transaction (enrollment inserts inside its own transaction, AD-7). It
// returns ErrWorkerExists for a known public key and ErrWorkerNameTaken for a
// name the owner already uses.
func (s *Store) InsertWorker(ctx context.Context, ex Execer, w WorkerRow) error {
	if ex == nil {
		ex = s.db
	}
	var firstConnected any
	if w.FirstConnectedAt != 0 {
		firstConnected = w.FirstConnectedAt
	}
	approval, approvedAt := "approved", any(w.CreatedAt)
	if w.Pending {
		approval, approvedAt = "pending", nil
	} else if w.ApprovedAt != 0 {
		approvedAt = w.ApprovedAt
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at, approval, enroll_addr, approved_at, approved_by, first_connected_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Pub, w.Name, w.OwnerID, w.Kind, w.CreatedAt, approval, w.EnrollAddr, approvedAt, w.ApprovedBy, firstConnected)
	if err != nil {
		switch msg := err.Error(); {
		case strings.Contains(msg, "workers.worker_pub"):
			return ErrWorkerExists
		case strings.Contains(msg, "workers.owner_id") && strings.Contains(msg, "workers.name"):
			return ErrWorkerNameTaken
		}
		return fmt.Errorf("store: insert worker: %w", err)
	}
	return nil
}

const workerColumns = `id, worker_pub, name, owner_id, kind, created_at, approval = 'pending', enroll_addr, COALESCE(approved_at, 0), approved_by, COALESCE(first_connected_at, 0), repair_until`

func scanWorker(row interface{ Scan(...any) error }) (WorkerRow, error) {
	var w WorkerRow
	err := row.Scan(&w.ID, &w.Pub, &w.Name, &w.OwnerID, &w.Kind, &w.CreatedAt, &w.Pending, &w.EnrollAddr, &w.ApprovedAt, &w.ApprovedBy, &w.FirstConnectedAt, &w.RepairUntil)
	return w, err
}

// ErrWorkerAlreadyApproved: the Worker is not pending.
var ErrWorkerAlreadyApproved = errors.New("store: the Worker is already approved")

// ApproveWorker marks a pending Worker approved. It returns ErrWorkerNotFound
// for an unknown id and ErrWorkerAlreadyApproved (with the row) when nothing
// was pending, so that only the first approval is audited. a, when set, is
// written in the same transaction (its Target is the Worker id).
func (s *Store) ApproveWorker(ctx context.Context, id, by string, nowMS int64, a *AuditRow) (WorkerRow, error) {
	var out WorkerRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		w, err := scanWorker(tx.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE id = ?`, id))
		if err := rowErr("read worker", err); err != nil {
			return err
		}
		out = w
		if !w.Pending {
			return ErrWorkerAlreadyApproved
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE workers SET approval = 'approved', approved_at = ?, approved_by = ? WHERE id = ? AND approval = 'pending'`,
			nowMS, by, id); err != nil {
			return fmt.Errorf("store: approve worker: %w", err)
		}
		out.Pending, out.ApprovedAt, out.ApprovedBy = false, nowMS, by
		return s.auditEnroll(ctx, tx, a, id)
	})
	return out, err
}

// WorkerByPub returns the Worker with the given public key, or ErrWorkerNotFound.
func (s *Store) WorkerByPub(ctx context.Context, pub []byte) (WorkerRow, error) {
	w, err := scanWorker(s.db.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE worker_pub = ?`, pub))
	return w, rowErr("read worker", err)
}

// WorkerByID returns the Worker with the given id, or ErrWorkerNotFound.
func (s *Store) WorkerByID(ctx context.Context, id string) (WorkerRow, error) {
	w, err := scanWorker(s.db.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE id = ?`, id))
	return w, rowErr("read worker", err)
}

// WorkerOwner returns the owner_id of a Worker, or ErrWorkerNotFound for an unknown
// or removed Worker. It reads the table on every call (no cache): the
// authorization decision depends on it (AD-11).
func (s *Store) WorkerOwner(ctx context.Context, id string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ?`, id).Scan(&owner)
	return owner, rowErr("read worker owner", err)
}

// TombstoneByPub returns the tombstone of a removed Worker key, or
// ErrWorkerNotFound when the key was never removed.
func (s *Store) TombstoneByPub(ctx context.Context, pub []byte) (TombstoneRow, error) {
	var t TombstoneRow
	err := s.db.QueryRowContext(ctx,
		`SELECT worker_id, worker_pub, name, owner_id, kind, created_at, removed_at, removed_by
		   FROM worker_tombstones WHERE worker_pub = ?`, pub).
		Scan(&t.WorkerID, &t.Pub, &t.Name, &t.OwnerID, &t.Kind, &t.CreatedAt, &t.RemovedAt, &t.RemovedBy)
	return t, rowErr("read tombstone", err)
}

// TombstoneWorkerTx turns the Worker row into a tombstone inside tx: the row
// is deleted and its key, name, owner and kind are kept in worker_tombstones.
// Story 4.10 calls it in the same transaction that removes the Worker's trust
// rows and bound keys. It returns ErrWorkerNotFound for an unknown id.
func (s *Store) TombstoneWorkerTx(ctx context.Context, tx *sql.Tx, id, removedBy string, removedAt int64) error {
	w, err := scanWorker(tx.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE id = ?`, id))
	if err := rowErr("read worker", err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workers WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete worker: %w", err)
	}
	// What the Worker last declared it shares goes with it: the history of a
	// removed Worker is not shown (nobody is authorised to read it), and its id
	// is never reused.
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_share WHERE worker_id = ?`, id); err != nil {
		return fmt.Errorf("store: delete worker share: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_share_pending WHERE worker_id = ?`, id); err != nil {
		return fmt.Errorf("store: delete worker share: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO worker_tombstones (worker_pub, worker_id, name, owner_id, kind, created_at, removed_at, removed_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		w.Pub, w.ID, w.Name, w.OwnerID, w.Kind, w.CreatedAt, removedAt, removedBy); err != nil {
		return fmt.Errorf("store: insert tombstone: %w", err)
	}
	return nil
}

// TombstoneWorker is TombstoneWorkerTx in a transaction of its own.
func (s *Store) TombstoneWorker(ctx context.Context, id, removedBy string, removedAt int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		return s.TombstoneWorkerTx(ctx, tx, id, removedBy, removedAt)
	})
}

func rowErr(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return ErrWorkerNotFound
	}
	return fmt.Errorf("store: %s: %w", what, err)
}
