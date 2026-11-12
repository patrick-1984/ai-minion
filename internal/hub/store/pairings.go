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
		if err := s.recordUsedCode(ctx, tx, id, codeDigest, w); err != nil {
			return err
		}
		return s.InsertWorker(ctx, tx, w)
	})
}

// recordUsedCode writes the digest of a used code together with the commit
// (pairing id, Worker key, Worker, owner and the code's expiry, copied from the
// pairing row) that lets the same Worker resume (hub-identity-v2 12.2).
func (s *Store) recordUsedCode(ctx context.Context, tx *sql.Tx, id, codeDigest string, w WorkerRow) error {
	var expires int64
	if err := tx.QueryRowContext(ctx, `SELECT expires_at FROM pending_pairings WHERE pairing_id = ?`, id).Scan(&expires); err != nil {
		return fmt.Errorf("store: read pairing expiry: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO used_pairing_codes
		   (code_digest, used_at, pairing_id, worker_pub, worker_id, owner_id, expires_at, resume_attempts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		codeDigest, w.CreatedAt, id, w.Pub, w.ID, w.OwnerID, expires)
	if err != nil {
		return fmt.Errorf("store: record used pairing code: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPairingCodeUsed
	}
	return nil
}

// PairingCommit is the recorded commit of a pairing code (the row of
// used_pairing_codes) that a resume relies on.
type PairingCommit struct {
	CodeDigest     string
	PairingID      string
	WorkerPub      []byte
	WorkerID       string
	OwnerID        string
	ExpiresAt      int64
	ResumeAttempts int
}

// MaxResumeAttempts is the number of wrong codes typed during a resume after
// which the resume is closed.
const MaxResumeAttempts = 5

// ErrNoPairingCommit: no live commit row matches the pairing id and key.
var ErrNoPairingCommit = errors.New("store: no pairing commit to resume")

// PairingCommitFor returns the commit of the pairing id made for exactly this
// Worker key, if it has not expired (expires_at > now) and fewer than
// MaxResumeAttempts wrong codes were typed. Rows from before migration 0023
// have no pairing data and never match.
func (s *Store) PairingCommitFor(ctx context.Context, pairingID string, workerPub []byte, now int64) (PairingCommit, error) {
	var c PairingCommit
	err := s.db.QueryRowContext(ctx,
		`SELECT code_digest, pairing_id, worker_pub, worker_id, owner_id, expires_at, resume_attempts
		   FROM used_pairing_codes
		  WHERE pairing_id = ? AND worker_pub = ? AND expires_at IS NOT NULL AND expires_at > ? AND resume_attempts < ?
		  ORDER BY used_at DESC LIMIT 1`,
		pairingID, workerPub, now, MaxResumeAttempts).
		Scan(&c.CodeDigest, &c.PairingID, &c.WorkerPub, &c.WorkerID, &c.OwnerID, &c.ExpiresAt, &c.ResumeAttempts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PairingCommit{}, ErrNoPairingCommit
	case err != nil:
		return PairingCommit{}, fmt.Errorf("store: read pairing commit: %w", err)
	}
	return c, nil
}

// AddResumeFailure counts one wrong code typed during a resume against the
// commit and returns the count after the change.
func (s *Store) AddResumeFailure(ctx context.Context, codeDigest string) (int, error) {
	var n int
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE used_pairing_codes SET resume_attempts = resume_attempts + 1 WHERE code_digest = ?`, codeDigest); err != nil {
			return fmt.Errorf("store: count resume attempt: %w", err)
		}
		return tx.QueryRowContext(ctx, `SELECT resume_attempts FROM used_pairing_codes WHERE code_digest = ?`, codeDigest).Scan(&n)
	})
	return n, err
}

// RepairPairing completes the owner re-issue of section 12.3 in one
// transaction: the new pending row becomes used, the new commit row is
// recorded, and the repair window of the existing Worker is cleared with a
// conditional update that must change exactly one row (the window is still
// open and the Worker never completed a connect handshake). It returns
// ErrRepairNotAllowed when the condition no longer holds; nothing changes.
func (s *Store) RepairPairing(ctx context.Context, id, codeDigest string, w WorkerRow, now int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE workers SET repair_until = 0
			  WHERE id = ? AND worker_pub = ? AND repair_until > ? AND first_connected_at IS NULL`, w.ID, w.Pub, now)
		if err != nil {
			return fmt.Errorf("store: close repair window: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrRepairNotAllowed
		}
		res, err = tx.ExecContext(ctx,
			`UPDATE pending_pairings SET status = 'used', owner_id = ?, worker_id = ? WHERE pairing_id = ? AND status = 'pending'`,
			w.OwnerID, w.ID, id)
		if err != nil {
			return fmt.Errorf("store: mark pairing used: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrPairingNotPending
		}
		return s.recordUsedCode(ctx, tx, id, codeDigest, w)
	})
}

// ErrRepairNotAllowed: the Worker is not (or no longer) eligible for the
// owner's pairing re-issue.
var ErrRepairNotAllowed = errors.New("store: the Worker is not eligible for a pairing re-issue")

// SetRepairWindow opens the owner's re-issue window of a Worker until the
// given time. The eligibility check and the write are one conditional update:
// the Worker must have a live row (a removed Worker has none, its tombstone is
// in another table) and must never have completed a connect handshake. It
// changes exactly one row or returns ErrRepairNotAllowed.
func (s *Store) SetRepairWindow(ctx context.Context, workerID string, until int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE workers SET repair_until = ?
			  WHERE id = ? AND first_connected_at IS NULL
			    AND NOT EXISTS (SELECT 1 FROM worker_tombstones t WHERE t.worker_pub = workers.worker_pub)`, until, workerID)
		if err != nil {
			return fmt.Errorf("store: open repair window: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrRepairNotAllowed
		}
		return nil
	})
}

// MarkFirstConnected records the first connect handshake of a Worker; later
// calls change nothing. It reports whether this call set it.
func (s *Store) MarkFirstConnected(ctx context.Context, workerID string, at int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE workers SET first_connected_at = ?, repair_until = 0 WHERE id = ? AND first_connected_at IS NULL`, at, workerID)
	if err != nil {
		return false, fmt.Errorf("store: mark first connect: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
