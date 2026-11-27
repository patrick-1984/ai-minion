package store

// Hidden sessions per Worker (v0.23.0), table worker_hidden_sessions. A Worker
// without a row has no choice of its own: the caller's account-wide
// preference (user_prefs.allow_hidden_sessions) decides for it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WorkerHiddenSessions returns the Worker's own hidden-sessions choice, nil
// when it has none (the account default applies). It does not check that the
// Worker exists.
func (s *Store) WorkerHiddenSessions(ctx context.Context, workerID string) (*bool, error) {
	var allow int
	err := s.db.QueryRowContext(ctx, `SELECT allow FROM worker_hidden_sessions WHERE worker_id = ?`, workerID).Scan(&allow)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read worker hidden sessions: %w", err)
	}
	v := allow != 0
	return &v, nil
}

// ListWorkerHiddenSessions returns the choice of every Worker that has one.
func (s *Store) ListWorkerHiddenSessions(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker_id, allow FROM worker_hidden_sessions`)
	if err != nil {
		return nil, fmt.Errorf("store: list worker hidden sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var allow int
		if err := rows.Scan(&id, &allow); err != nil {
			return nil, fmt.Errorf("store: list worker hidden sessions: %w", err)
		}
		out[id] = allow != 0
	}
	return out, rows.Err()
}

// SetWorkerHiddenSessions stores the Worker's own hidden-sessions choice, or
// removes it when allow is nil (the account default applies again). by is the
// actor. It returns [ErrWorkerNotFound] for an unknown or removed Worker.
func (s *Store) SetWorkerHiddenSessions(ctx context.Context, workerID string, allow *bool, by string) error {
	return s.SetWorkerHiddenSessionsIfOwner(ctx, workerID, allow, by, "")
}

// SetWorkerHiddenSessionsIfOwner is [Store.SetWorkerHiddenSessions] made
// conditional on the owner of the Worker, in one transaction: with
// expectedOwnerID set, a Worker that is owned by someone else when the
// choice is written (reassigned after the caller was found to be its owner)
// returns [ErrWorkerOwnerChanged] and nothing is stored. "" checks no owner
// (a caller allowed by manage, which does not depend on the owner).
func (s *Store) SetWorkerHiddenSessionsIfOwner(ctx context.Context, workerID string, allow *bool, by, expectedOwnerID string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ?`, workerID).Scan(&owner)
		if err := rowErr("read worker", err); err != nil {
			return err
		}
		if expectedOwnerID != "" && owner != expectedOwnerID {
			return ErrWorkerOwnerChanged
		}
		if allow == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM worker_hidden_sessions WHERE worker_id = ?`, workerID); err != nil {
				return fmt.Errorf("store: clear worker hidden sessions: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO worker_hidden_sessions (worker_id, allow, updated_by, updated_at) VALUES (?1, ?2, ?3, ?4)
			 ON CONFLICT (worker_id) DO UPDATE SET allow = ?2, updated_by = ?3, updated_at = ?4`,
			workerID, boolInt(*allow), by, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("store: set worker hidden sessions: %w", err)
		}
		return nil
	})
}
