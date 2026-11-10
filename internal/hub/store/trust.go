package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Errors of the trust and Worker-kind statements. Callers match them with
// errors.Is. ErrWorkerNotFound, ErrWorkerNameTaken, ErrNotFound and
// ErrUserInactive are declared next to the tables they belong to.
var (
	// ErrWorkerNotInfrastructure: the Worker is private, and only an
	// infrastructure Worker can be reassigned (AD-17).
	ErrWorkerNotInfrastructure = errors.New("store: only an infrastructure Worker can be reassigned")
	// ErrWorkerSameOwner: the Worker already belongs to the named user.
	ErrWorkerSameOwner = errors.New("store: the Worker already belongs to this user")
	// ErrWorkerOwnerChanged: the Worker is not owned by the user the caller
	// based its decision on; it was reassigned in between.
	ErrWorkerOwnerChanged = errors.New("store: the Worker changed owner")
)

// TrustRow says that a receiving user reads the output of a Worker (or of one
// of its sessions) as trusted (AD-17).
type TrustRow struct {
	ReceiverID string
	WorkerID   string
	// SessionName is empty for a row that covers the whole Worker.
	SessionName string
	CreatedAt   int64 // unix milliseconds
}

// PutTrust stores a trust row. Storing a row that exists changes nothing, so
// the call is idempotent and keeps the original creation time.
func (s *Store) PutTrust(ctx context.Context, r TrustRow) error {
	if r.ReceiverID == "" || r.WorkerID == "" {
		return errors.New("store: a trust row needs a receiver and a Worker")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO trust (receiver_user_id, worker_id, session_name, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (receiver_user_id, worker_id, session_name) DO NOTHING`,
		r.ReceiverID, r.WorkerID, r.SessionName, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: put trust: %w", err)
	}
	return nil
}

// PutTrustIfOwner is [Store.PutTrust] made conditional on the owner of the
// Worker: in one transaction it reads the Worker's owner and stores the row
// only when it is expectedOwnerID. A reassignment deletes every trust row of
// the Worker in its own transaction, so a mark that was decided under the
// previous owner can never be stored after it: the call returns
// [ErrWorkerOwnerChanged] (or [ErrWorkerNotFound] for an unknown or removed
// Worker) and stores nothing.
func (s *Store) PutTrustIfOwner(ctx context.Context, r TrustRow, expectedOwnerID string) error {
	if r.ReceiverID == "" || r.WorkerID == "" || expectedOwnerID == "" {
		return errors.New("store: a trust row needs a receiver, a Worker and an owner")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var owner string
		if err := rowErr("read worker owner", tx.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ?`, r.WorkerID).Scan(&owner)); err != nil {
			return err
		}
		if owner != expectedOwnerID {
			return ErrWorkerOwnerChanged
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO trust (receiver_user_id, worker_id, session_name, created_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (receiver_user_id, worker_id, session_name) DO NOTHING`,
			r.ReceiverID, r.WorkerID, r.SessionName, r.CreatedAt); err != nil {
			return fmt.Errorf("store: put trust: %w", err)
		}
		return nil
	})
}

// DeleteTrust removes exactly the row (receiver, worker, session); a session
// of "" is the whole-Worker row, and session rows of the same Worker stay. It
// reports whether a row existed.
func (s *Store) DeleteTrust(ctx context.Context, receiverID, workerID, sessionName string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM trust WHERE receiver_user_id = ? AND worker_id = ? AND session_name = ?`,
		receiverID, workerID, sessionName)
	if err != nil {
		return false, fmt.Errorf("store: delete trust: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete trust: %w", err)
	}
	return n > 0, nil
}

// HasTrust reports whether the receiver trusts the session: a row for the
// session itself or one for the whole Worker. With sessionName "" only the
// whole-Worker row counts. It reads the table on every call (no cache).
func (s *Store) HasTrust(ctx context.Context, receiverID, workerID, sessionName string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM trust WHERE receiver_user_id = ? AND worker_id = ? AND (session_name = '' OR session_name = ?) LIMIT 1`,
		receiverID, workerID, sessionName).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read trust: %w", err)
	}
	return true, nil
}

// ListTrust returns the rows of one receiver, ordered by Worker and session.
func (s *Store) ListTrust(ctx context.Context, receiverID string) ([]TrustRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT receiver_user_id, worker_id, session_name, created_at FROM trust
		  WHERE receiver_user_id = ? ORDER BY worker_id, session_name`, receiverID)
	if err != nil {
		return nil, fmt.Errorf("store: list trust: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TrustRow
	for rows.Next() {
		var r TrustRow
		if err := rows.Scan(&r.ReceiverID, &r.WorkerID, &r.SessionName, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list trust: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list trust: %w", err)
	}
	return out, nil
}

// DeleteTrustOfWorker deletes every trust row of a Worker and its sessions,
// whoever the receiver is, on ex: the open transaction of a reassignment or a
// removal (AD-16). It returns the number of rows deleted.
func (s *Store) DeleteTrustOfWorker(ctx context.Context, ex Execer, workerID string) (int64, error) {
	if ex == nil {
		ex = s.db
	}
	res, err := ex.ExecContext(ctx, `DELETE FROM trust WHERE worker_id = ?`, workerID)
	if err != nil {
		return 0, fmt.Errorf("store: delete trust of worker: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete trust of worker: %w", err)
	}
	return n, nil
}

// ReassignParams describes the reassignment of an infrastructure Worker.
type ReassignParams struct {
	WorkerID   string
	NewOwnerID string
	// AtMS is the time keys are revoked at (unix milliseconds).
	AtMS int64
	// Audit, when not nil, is written in the same transaction; its Target is
	// filled with the Worker id when empty.
	Audit *AuditRow
}

// ReassignResult tells what a reassignment changed.
type ReassignResult struct {
	// Worker is the Worker after the change.
	Worker          WorkerRow
	PreviousOwnerID string
	// TrustDeleted and KeysRevoked count the trust rows deleted and the
	// bound API keys revoked.
	TrustDeleted int64
	KeysRevoked  int64
}

// ReassignWorker moves an infrastructure Worker to another user in one
// transaction (AD-16, AD-17): the owner changes, every trust row of the Worker
// and its sessions is deleted (the admin's included), the API keys bound to
// the Worker are revoked (AD-11) and the audit row is written. It returns
//
//   - [ErrWorkerNotFound] for an unknown Worker;
//   - [ErrWorkerNotInfrastructure] for a private Worker;
//   - [ErrNotFound] and [ErrUserInactive] for an unknown or deactivated new
//     owner;
//   - [ErrWorkerSameOwner] when the owner would not change;
//   - [ErrWorkerNameTaken] when the new owner already has a Worker of this
//     name.
//
// Nothing changes on any error.
func (s *Store) ReassignWorker(ctx context.Context, p ReassignParams) (ReassignResult, error) {
	var res ReassignResult
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		w, err := scanWorker(tx.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE id = ?`, p.WorkerID))
		if err := rowErr("read worker", err); err != nil {
			return err
		}
		if w.Kind != WorkerKindInfrastructure {
			return ErrWorkerNotInfrastructure
		}
		var active int
		switch err := tx.QueryRowContext(ctx, `SELECT active FROM users WHERE id = ?`, p.NewOwnerID).Scan(&active); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: read new owner: %w", err)
		case active == 0:
			return ErrUserInactive
		}
		if w.OwnerID == p.NewOwnerID {
			return ErrWorkerSameOwner
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workers SET owner_id = ? WHERE id = ?`, p.NewOwnerID, p.WorkerID); err != nil {
			if nameCollision(err) {
				return ErrWorkerNameTaken
			}
			return fmt.Errorf("store: reassign worker: %w", err)
		}
		res = ReassignResult{Worker: w, PreviousOwnerID: w.OwnerID}
		res.Worker.OwnerID = p.NewOwnerID
		if res.TrustDeleted, err = s.DeleteTrustOfWorker(ctx, tx, p.WorkerID); err != nil {
			return err
		}
		if res.KeysRevoked, err = s.RevokeAPIKeysBoundToWorker(ctx, tx, p.WorkerID, p.AtMS); err != nil {
			return err
		}
		if p.Audit != nil {
			row := *p.Audit
			if row.Target == "" {
				row.Target = p.WorkerID
			}
			return s.InsertAudit(ctx, tx, row)
		}
		return nil
	})
	if err != nil {
		return ReassignResult{}, err
	}
	return res, nil
}

// RenameWorker changes the name of a Worker. It returns [ErrWorkerNotFound]
// for an unknown Worker and [ErrWorkerNameTaken] when its owner already has a
// Worker of that name. Renaming to the current name succeeds.
func (s *Store) RenameWorker(ctx context.Context, workerID, name string) (WorkerRow, error) {
	var out WorkerRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		w, err := scanWorker(tx.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE id = ?`, workerID))
		if err := rowErr("read worker", err); err != nil {
			return err
		}
		if w.Name != name {
			if _, err := tx.ExecContext(ctx, `UPDATE workers SET name = ? WHERE id = ?`, name, workerID); err != nil {
				if nameCollision(err) {
					return ErrWorkerNameTaken
				}
				return fmt.Errorf("store: rename worker: %w", err)
			}
		}
		w.Name = name
		out = w
		return nil
	})
	return out, err
}

// SetWorkerPreviewOnly sets the Hub-side preview_only policy of a Worker and
// reports whether the stored value changed. It returns [ErrWorkerNotFound] for
// an unknown Worker.
func (s *Store) SetWorkerPreviewOnly(ctx context.Context, workerID string, on bool) (changed bool, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var cur int
		if err := rowErr("read worker policy", tx.QueryRowContext(ctx, `SELECT preview_only FROM workers WHERE id = ?`, workerID).Scan(&cur)); err != nil {
			return err
		}
		if (cur != 0) == on {
			return nil
		}
		changed = true
		if _, err := tx.ExecContext(ctx, `UPDATE workers SET preview_only = ? WHERE id = ?`, boolInt(on), workerID); err != nil {
			return fmt.Errorf("store: set worker policy: %w", err)
		}
		return nil
	})
	return changed, err
}

// WorkerPreviewOnly returns the Hub-side preview_only policy of a Worker, read
// from the table on every call, or [ErrWorkerNotFound].
func (s *Store) WorkerPreviewOnly(ctx context.Context, workerID string) (bool, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT preview_only FROM workers WHERE id = ?`, workerID).Scan(&v)
	return v != 0, rowErr("read worker policy", err)
}

// nameCollision reports whether err is the unique violation of
// workers (owner_id, name).
func nameCollision(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "workers.owner_id") && strings.Contains(msg, "workers.name")
}
