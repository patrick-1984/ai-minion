package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// ErrShareRevision is the error of [Store.ApplyShareChangeContext] when the
// saved selection is no longer at the revision the caller read: nothing was
// changed.
var ErrShareRevision = errors.New("store: the share selection changed since it was read")

// ShareChange is a change of the share selection (tray W6a): an optional new
// mode, then sessions added to and removed from the selected list. A change
// with Add or Remove needs the resulting mode select. IfRevision, when not
// zero, is the revision the caller read; zero applies without the check.
type ShareChange struct {
	Mode       ShareMode
	Add        []string
	Remove     []string
	IfRevision uint64
}

// ApplyShareChangeContext applies c in one transaction: it reads the saved
// selection and its revision, checks IfRevision, computes the new selection
// and commits it with the next revision. Select with an empty list is allowed
// (nothing shared).
func (s *Store) ApplyShareChangeContext(ctx context.Context, c ShareChange) (ShareState, error) {
	for _, n := range append(slices.Clone(c.Add), c.Remove...) {
		if n == "" {
			return ShareState{}, fmt.Errorf("%w: empty session name", ErrInvalidShare)
		}
	}
	if c.Mode != "" && !c.Mode.Valid() {
		return ShareState{}, fmt.Errorf("%w: unknown mode %q (want all, none, select or later)", ErrInvalidShare, c.Mode)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var mode sql.NullString
	var rev sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT c.mode, r.revision FROM (SELECT 1) AS one
		 LEFT JOIN share_config AS c ON c.id = 1
		 LEFT JOIN share_revision AS r ON r.id = 1`).Scan(&mode, &rev); err != nil {
		return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
	}
	cur := uint64(0)
	if rev.Valid && rev.Int64 > 0 {
		cur = uint64(rev.Int64)
	}
	if c.IfRevision != 0 && c.IfRevision != cur {
		return ShareState{}, fmt.Errorf("%w (read %d, now %d)", ErrShareRevision, c.IfRevision, cur)
	}
	next := ShareLater
	if mode.Valid {
		next = ShareMode(mode.String)
	}
	if c.Mode != "" {
		next = c.Mode
	}
	if (len(c.Add) > 0 || len(c.Remove) > 0) && next != ShareSelect {
		return ShareState{}, fmt.Errorf("%w: adding or removing sessions needs mode select (the mode is %s)", ErrInvalidShare, next)
	}
	var names []string
	if next == ShareSelect {
		rows, err := tx.QueryContext(ctx, `SELECT session_name FROM share_selected ORDER BY position`)
		if err != nil {
			return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()
				return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
			}
			names = append(names, n)
		}
		_ = rows.Close()
		if c.Mode == ShareSelect && mode.String != string(ShareSelect) {
			names = nil // a new select starts from the names given
		}
		for _, n := range c.Add {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		names = slices.DeleteFunc(names, func(n string) bool { return slices.Contains(c.Remove, n) })
	}
	now := s.clock.now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO share_config (id, mode, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`,
		string(next), now); err != nil {
		return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM share_selected`); err != nil {
		return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
	}
	for i, n := range names {
		if _, err := tx.ExecContext(ctx, `INSERT INTO share_selected (session_name, position) VALUES (?, ?)`, n, i); err != nil {
			return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
		}
	}
	newRev, err := bumpShareRevision(ctx, tx)
	if err != nil {
		return ShareState{}, err
	}
	if err := tx.Commit(); err != nil {
		return ShareState{}, fmt.Errorf("store: change share selection: %w", err)
	}
	if names == nil {
		names = []string{}
	}
	return ShareState{ShareSelection: ShareSelection{Mode: next, Sessions: names, Configured: true}, Revision: newRev}, nil
}
