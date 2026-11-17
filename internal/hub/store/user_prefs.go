package store

// Per-user preferences (v0.10.0), table user_prefs. A user without a row has
// the defaults; the row is written by the user's own account_prefs_set.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UserPrefs are the preferences of one user.
type UserPrefs struct {
	// AllowHiddenSessions lets the user create sessions that they could not
	// see or join from their own terminal, when the Hub setting
	// hidden_sessions is user_controlled. Off by default.
	AllowHiddenSessions bool
	// UpdatedAt is when the user last changed a preference (unix
	// milliseconds); zero for the defaults.
	UpdatedAt int64
}

// UserPrefs returns the preferences of userID, or the defaults when the user
// never changed one. It does not check that the user exists.
func (s *Store) UserPrefs(ctx context.Context, userID string) (UserPrefs, error) {
	var p UserPrefs
	var allow int
	err := s.db.QueryRowContext(ctx,
		`SELECT allow_hidden_sessions, updated_at FROM user_prefs WHERE user_id = ?`, userID).Scan(&allow, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return UserPrefs{}, nil
	}
	if err != nil {
		return UserPrefs{}, fmt.Errorf("store: read user preferences: %w", err)
	}
	p.AllowHiddenSessions = allow != 0
	return p, nil
}

// SetAllowHiddenSessions stores the hidden-sessions preference of userID. It
// returns [ErrNotFound] when the user does not exist.
func (s *Store) SetAllowHiddenSessions(ctx context.Context, userID string, allow bool) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read user: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_prefs (user_id, allow_hidden_sessions, updated_at) VALUES (?1, ?2, ?3)
			 ON CONFLICT (user_id) DO UPDATE SET allow_hidden_sessions = ?2, updated_at = ?3`,
			userID, boolInt(allow), time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("store: set user preferences: %w", err)
		}
		return nil
	})
}
