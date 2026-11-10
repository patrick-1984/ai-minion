package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrActAsActive is returned by [Store.StartActAs] when the browser session
// is already acting as a user.
var ErrActAsActive = errors.New("store: the session is already acting as a user")

// ActAsRow is one row of the act_as_sessions table: the admin (ActorID) who
// owns the browser session SessionID acts as TargetID until ExpiresAt.
type ActAsRow struct {
	SessionID string // the sessions.id of the admin's browser session
	ActorID   string
	TargetID  string
	StartedAt int64 // unix milliseconds
	ExpiresAt int64
}

// ActAsInfo is an [ActAsRow] with the username of the target, which the
// banner shows, and whether the target account is still active.
type ActAsInfo struct {
	ActAsRow
	TargetUsername string
	TargetActive   bool
}

// StartActAs stores a new act-as row. It returns [ErrActAsActive] when the
// session already has one (the check and the insert share one transaction, so
// two requests cannot both start).
func (s *Store) StartActAs(ctx context.Context, row ActAsRow) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM act_as_sessions WHERE session_id = ?`, row.SessionID).Scan(&one); {
		case err == nil:
			return ErrActAsActive
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: check act-as: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO act_as_sessions (session_id, actor_id, target_id, started_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			row.SessionID, row.ActorID, row.TargetID, row.StartedAt, row.ExpiresAt); err != nil {
			return fmt.Errorf("store: start act-as: %w", err)
		}
		return nil
	})
}

// ActAsBySession returns the act-as state of a browser session, or
// [ErrNotFound]. It does not judge expiry; the session package does.
func (s *Store) ActAsBySession(ctx context.Context, sessionID string) (ActAsInfo, error) {
	var info ActAsInfo
	err := s.db.QueryRowContext(ctx, `
SELECT a.session_id, a.actor_id, a.target_id, a.started_at, a.expires_at, t.username, t.active
FROM act_as_sessions a JOIN users t ON t.id = a.target_id
WHERE a.session_id = ?`, sessionID).Scan(
		&info.SessionID, &info.ActorID, &info.TargetID, &info.StartedAt, &info.ExpiresAt, &info.TargetUsername, &info.TargetActive)
	if errors.Is(err, sql.ErrNoRows) {
		return ActAsInfo{}, ErrNotFound
	}
	if err != nil {
		return ActAsInfo{}, fmt.Errorf("store: read act-as: %w", err)
	}
	return info, nil
}

// EndActAs deletes the act-as row of a session and reports whether one
// existed. It is the stop that the admin asks for; the automatic ends use
// [Store.EndActAsIf], which cannot hit a newer period.
func (s *Store) EndActAs(ctx context.Context, sessionID string) (bool, error) {
	var ended bool
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM act_as_sessions WHERE session_id = ?`, sessionID)
		if err != nil {
			return fmt.Errorf("store: end act-as: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: end act-as: %w", err)
		}
		ended = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return ended, nil
}

// EndActAsIf deletes the act-as period that the caller observed (row: the
// session, the target and both times) and reports whether it deleted that
// period. A period that was removed and replaced by a new start in the
// meantime does not match, so a request that read an old period can never end
// the new one. When a row was deleted the audit row is written in the same
// transaction, so the end is recorded exactly once even when two requests
// find the same period (AD-16).
func (s *Store) EndActAsIf(ctx context.Context, row ActAsRow, audit AuditRow) (bool, error) {
	var ended bool
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
DELETE FROM act_as_sessions
WHERE session_id = ? AND actor_id = ? AND target_id = ? AND started_at = ? AND expires_at = ?`,
			row.SessionID, row.ActorID, row.TargetID, row.StartedAt, row.ExpiresAt)
		if err != nil {
			return fmt.Errorf("store: end act-as: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: end act-as: %w", err)
		}
		if ended = n > 0; ended {
			return s.InsertAudit(ctx, tx, audit)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return ended, nil
}
