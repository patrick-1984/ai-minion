package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrCommitNotDurable is returned (wrapped) by [Store.RotateHubIdentity] when
// the rotation committed but could not be forced to disk. The rotation stands.
var ErrCommitNotDurable = errors.New("store: the rotation committed but is not known to be on disk")

// ErrRotationConflict is returned by [Store.RotateHubIdentity] when the
// recorded Hub identity or the end of the transition chain is not the one the
// caller based the rotation on: another rotation ran first.
var ErrRotationConflict = errors.New("store: the Hub identity changed meanwhile")

// TransitionRow is one link of the Hub key transition chain (AD-5). The keys
// and the signature are raw bytes; the workers package turns them into
// identity.Transition values.
type TransitionRow struct {
	Seq       uint64 // 1 for the first rotation
	OldPub    []byte // 32 bytes
	NewPub    []byte // 32 bytes
	CreatedAt int64  // unix milliseconds
	SigByOld  []byte // 64 bytes
}

// RotateParams describes one rotation, applied by [Store.RotateHubIdentity] in
// a single transaction.
type RotateParams struct {
	// Expect is the Hub public key the rotation was based on; the rotation is
	// refused with [ErrRotationConflict] unless it is the recorded identity.
	Expect []byte
	// Transition is the new link. Its Seq must follow the last link and its
	// OldPub must be Expect.
	Transition TransitionRow
	// Identity replaces the recorded hub_identity row.
	Identity HubIdentityRow
	// Audit, when set, is written in the same transaction (AD-16).
	Audit *AuditRow
}

const transitionColumns = `seq, old_pub, new_pub, created_at, sig_by_old`

func scanTransitions(rows *sql.Rows) ([]TransitionRow, error) {
	defer func() { _ = rows.Close() }()
	var out []TransitionRow
	for rows.Next() {
		var (
			r   TransitionRow
			seq int64
		)
		if err := rows.Scan(&seq, &r.OldPub, &r.NewPub, &r.CreatedAt, &r.SigByOld); err != nil {
			return nil, fmt.Errorf("store: scan key transition: %w", err)
		}
		r.Seq = uint64(seq)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read key transitions: %w", err)
	}
	return out, nil
}

// Transitions returns the whole transition chain, ordered by seq. It is empty
// when the Hub identity was never rotated.
func (s *Store) Transitions(ctx context.Context) ([]TransitionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+transitionColumns+` FROM hub_key_transitions ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("store: read key transitions: %w", err)
	}
	return scanTransitions(rows)
}

// TransitionsFrom returns the chain from the link that hands over from pub
// forward, ordered by seq: what a Worker pinned to pub has to follow. It is
// empty when pub is not the old key of any link (the Worker is already at the
// end of the chain, or does not know this Hub).
func (s *Store) TransitionsFrom(ctx context.Context, pub []byte) ([]TransitionRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+transitionColumns+` FROM hub_key_transitions
		 WHERE seq >= (SELECT seq FROM hub_key_transitions WHERE old_pub = ?) ORDER BY seq`, pub)
	if err != nil {
		return nil, fmt.Errorf("store: read key transitions: %w", err)
	}
	return scanTransitions(rows)
}

// LastTransition returns the newest link, or ok=false when there is none.
func (s *Store) LastTransition(ctx context.Context) (row TransitionRow, ok bool, err error) {
	var seq int64
	err = s.db.QueryRowContext(ctx,
		`SELECT `+transitionColumns+` FROM hub_key_transitions ORDER BY seq DESC LIMIT 1`).
		Scan(&seq, &row.OldPub, &row.NewPub, &row.CreatedAt, &row.SigByOld)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TransitionRow{}, false, nil
	case err != nil:
		return TransitionRow{}, false, fmt.Errorf("store: read last key transition: %w", err)
	}
	row.Seq = uint64(seq)
	return row, true, nil
}

// RotateHubIdentity appends a transition link and replaces the recorded Hub
// identity in one transaction, with the audit row when given. The identity
// is permanent otherwise ([Store.PutHubIdentity] never replaces it); this is
// the only way it changes. It returns [ErrRotationConflict] when the recorded
// identity is not p.Expect or p.Transition does not extend the chain by one.
//
// The database runs WAL with synchronous=NORMAL, where a commit is not on
// disk until the next checkpoint. A caller replaces the private identity key
// right after this returns, so the rotation is forced to disk here: when that
// fails the commit stands and the error wraps [ErrCommitNotDurable].
func (s *Store) RotateHubIdentity(ctx context.Context, p RotateParams) error {
	if err := s.rotateHubIdentity(ctx, p); err != nil {
		return err
	}
	return s.MakeDurable(ctx)
}

// MakeDurable forces every committed transaction to disk (a full WAL
// checkpoint) and returns an error that wraps [ErrCommitNotDurable] when it
// cannot prove that it did. Callers use it to confirm, or to retry the
// confirmation of, a commit that later steps (a private key replacement, the
// publication of a new identity) must not outrun. It is safe to call at any
// time and idempotent.
func (s *Store) MakeDurable(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	// FULL syncs the WAL, copies it into the database file and syncs that.
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("%w: %v", ErrCommitNotDurable, err)
	}
	if busy != 0 {
		return fmt.Errorf("%w: the checkpoint was blocked by another connection", ErrCommitNotDurable)
	}
	return nil
}

func (s *Store) rotateHubIdentity(ctx context.Context, p RotateParams) error {
	t := p.Transition
	if len(p.Expect) == 0 || !bytes.Equal(t.OldPub, p.Expect) || !bytes.Equal(t.NewPub, p.Identity.Pub) {
		return errors.New("store: the rotation does not connect the expected key to the new identity")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var cur []byte
		err := tx.QueryRowContext(ctx, `SELECT pub FROM hub_identity WHERE id = 1`).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !bytes.Equal(cur, p.Expect)) {
			return ErrRotationConflict
		}
		if err != nil {
			return fmt.Errorf("store: read hub identity: %w", err)
		}
		var last int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM hub_key_transitions`).Scan(&last); err != nil {
			return fmt.Errorf("store: read key transitions: %w", err)
		}
		if t.Seq != uint64(last)+1 {
			return ErrRotationConflict
		}
		var reused int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM hub_key_transitions WHERE old_pub = ?1 OR new_pub = ?1`, t.NewPub).Scan(&reused); err != nil {
			return fmt.Errorf("store: read key transitions: %w", err)
		}
		if reused > 0 || bytes.Equal(t.NewPub, cur) {
			return errors.New("store: the new Hub identity key was already part of the chain; a rotation needs a fresh key")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO hub_key_transitions (seq, old_pub, new_pub, created_at, sig_by_old) VALUES (?, ?, ?, ?, ?)`,
			int64(t.Seq), t.OldPub, t.NewPub, t.CreatedAt, t.SigByOld); err != nil {
			return fmt.Errorf("store: append key transition: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE hub_identity SET pub = ?, fingerprint = ?, created_at = ? WHERE id = 1`,
			p.Identity.Pub, p.Identity.Fingerprint, p.Identity.CreatedAt); err != nil {
			return fmt.Errorf("store: replace hub identity: %w", err)
		}
		if p.Audit != nil {
			if err := s.InsertAudit(ctx, tx, *p.Audit); err != nil {
				return err
			}
		}
		return nil
	})
}
