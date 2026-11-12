package store

import (
	"context"
	"fmt"
)

// PairingReset reports what [Store.ResetPairing] removed.
type PairingReset struct {
	// Enrollment is true when a Hub pin and enrollment existed.
	Enrollment bool
	// Share is true when a share choice (mode or selected sessions) existed.
	Share bool
}

// ResetPairing deletes the Hub pin and enrollment and the share selection in
// one transaction, for `minion worker reset` (AD-9, R81), and the turn outbox
// of the old enrollment with them (Story 7.3). Nothing else is touched: the
// session counter (a name is never reused), the result cache, the local audit
// and the Solo pipe stay, so the tmux sessions and their numbering are not
// disturbed. It is idempotent: a second call reports nothing removed.
func (s *Store) ResetPairing(ctx context.Context) (PairingReset, error) {
	var out PairingReset
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("store: reset pairing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM hub_enrollment`)
	if err != nil {
		return out, fmt.Errorf("store: reset pairing: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return out, fmt.Errorf("store: reset pairing: %w", err)
	}
	out.Enrollment = n > 0

	for _, table := range []string{"share_config", "share_selected"} {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+table)
		if err != nil {
			return out, fmt.Errorf("store: reset pairing: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			out.Share = true
		}
	}
	// Removing the share choice is a change of it: the Hub orders the rules of
	// a Worker by the revision, which a reset must not take back.
	if out.Share {
		if _, err := bumpShareRevision(ctx, tx); err != nil {
			return out, err
		}
	}
	// A pairing in progress ends with the reset: its resume record goes, so that
	// a new code is shown (design section 12.2).
	if _, err := tx.ExecContext(ctx, `DELETE FROM pair_resume`); err != nil {
		return out, fmt.Errorf("store: reset pairing: %w", err)
	}
	// The turns the old Hub never acknowledged belong to the old enrollment: a
	// new Hub must not receive them (Story 7.3).
	if _, err := tx.ExecContext(ctx, `DELETE FROM pipe_outbox`); err != nil {
		return out, fmt.Errorf("store: reset pairing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("store: reset pairing: %w", err)
	}
	return out, nil
}
