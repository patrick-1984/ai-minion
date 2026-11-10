package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Pairing statuses as stored (AD-8).
const (
	// PairingPending: the code can still be claimed.
	PairingPending = "pending"
	// PairingUsed: the code created a Worker and cannot be used again.
	PairingUsed = "used"
	// PairingDropped: the attempt limit was reached; the code is dead.
	PairingDropped = "dropped"
)

// Errors of the pending_pairings table. Callers match them with errors.Is.
var (
	// ErrPairingNotFound: no row for this pairing id.
	ErrPairingNotFound = errors.New("store: pairing not found")
	// ErrPairingNotPending: the row is used or dropped (or gone), so it cannot
	// be completed or charged with a failed attempt.
	ErrPairingNotPending = errors.New("store: pairing is not pending")
	// ErrPairingCodeUsed: the code (its digest) already created a Worker.
	ErrPairingCodeUsed = errors.New("store: pairing code was already used")
)

// PairingRow is the persistent state of one public pairing id. It holds no
// secret: the code, its password and the keys never reach the database.
type PairingRow struct {
	ID        string // the public pairing id (4 characters)
	Status    string // PairingPending, PairingUsed or PairingDropped
	Attempts  int    // failed claims so far
	CreatedAt int64  // unix milliseconds
	ExpiresAt int64  // unix milliseconds
	OwnerID   string // when used: the claiming user
	WorkerID  string // when used: the Worker that was created
}

const pairingColumns = `pairing_id, status, attempts, created_at, expires_at, owner_id, worker_id`

func scanPairing(row interface{ Scan(...any) error }) (PairingRow, error) {
	var p PairingRow
	err := row.Scan(&p.ID, &p.Status, &p.Attempts, &p.CreatedAt, &p.ExpiresAt, &p.OwnerID, &p.WorkerID)
	return p, err
}

// Pairing returns the row of a public pairing id, or ErrPairingNotFound.
func (s *Store) Pairing(ctx context.Context, id string) (PairingRow, error) {
	p, err := scanPairing(s.db.QueryRowContext(ctx, `SELECT `+pairingColumns+` FROM pending_pairings WHERE pairing_id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PairingRow{}, ErrPairingNotFound
	case err != nil:
		return PairingRow{}, fmt.Errorf("store: read pairing: %w", err)
	}
	return p, nil
}

// PutPairing inserts the row or replaces the existing row of the same id. The
// workers package calls it to start a code, and to start a new one over an
// expired row.
func (s *Store) PutPairing(ctx context.Context, p PairingRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_pairings (`+pairingColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(pairing_id) DO UPDATE SET
		   status = excluded.status, attempts = excluded.attempts, created_at = excluded.created_at,
		   expires_at = excluded.expires_at, owner_id = excluded.owner_id, worker_id = excluded.worker_id`,
		p.ID, p.Status, p.Attempts, p.CreatedAt, p.ExpiresAt, p.OwnerID, p.WorkerID)
	if err != nil {
		return fmt.Errorf("store: write pairing: %w", err)
	}
	return nil
}

// AddPairingFailure charges one failed attempt to a pending pairing in a
// single statement (two concurrent failures never lose a count) and drops the
// pairing when the count reaches max. It returns the row after the change, or
// ErrPairingNotPending when the row is not pending.
func (s *Store) AddPairingFailure(ctx context.Context, id string, max int) (PairingRow, error) {
	var out PairingRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE pending_pairings
			    SET attempts = attempts + 1,
			        status = CASE WHEN attempts + 1 >= ? THEN 'dropped' ELSE status END
			  WHERE pairing_id = ? AND status = 'pending'`, max, id)
		if err != nil {
			return fmt.Errorf("store: charge pairing attempt: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrPairingNotPending
		}
		out, err = scanPairing(tx.QueryRowContext(ctx, `SELECT `+pairingColumns+` FROM pending_pairings WHERE pairing_id = ?`, id))
		if err != nil {
			return fmt.Errorf("store: read pairing: %w", err)
		}
		return nil
	})
	return out, err
}

// CompletePairing creates the Worker, marks the pairing used and records the
// digest of the code that was used, in one transaction, so a code can create
// at most one Worker, now or after its row expired. It returns
// ErrPairingNotPending when the row is not pending (used, dropped or gone),
// ErrPairingCodeUsed when the digest was recorded before, and the errors of
// [Store.InsertWorker] (ErrWorkerExists, ErrWorkerNameTaken) without changing
// anything.
func (s *Store) CompletePairing(ctx context.Context, id, codeDigest string, w WorkerRow) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE pending_pairings SET status = 'used', owner_id = ?, worker_id = ? WHERE pairing_id = ? AND status = 'pending'`,
			w.OwnerID, w.ID, id)
		if err != nil {
			return fmt.Errorf("store: mark pairing used: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrPairingNotPending
		}
		res, err = tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO used_pairing_codes (code_digest, used_at) VALUES (?, ?)`,
			codeDigest, w.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: record used pairing code: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrPairingCodeUsed
		}
		return s.InsertWorker(ctx, tx, w)
	})
}

// PairingCodeUsed reports whether the code with this digest already created a
// Worker. The digest outlives the pending_pairings row.
func (s *Store) PairingCodeUsed(ctx context.Context, codeDigest string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM used_pairing_codes WHERE code_digest = ?`, codeDigest).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read used pairing code: %w", err)
	}
	return true, nil
}

// PurgePairings deletes the rows that expired before the given time (unix
// milliseconds) and returns how many it removed. An expired row no longer
// protects anything: a new code may reuse its id.
func (s *Store) PurgePairings(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pending_pairings WHERE expires_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("store: purge pairings: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
