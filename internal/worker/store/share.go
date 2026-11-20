package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ShareMode says which of the Worker's tmux sessions the Hub may see (R75).
type ShareMode string

// The four share modes of the first-run question.
const (
	// ShareAll shares every session, including sessions created later.
	ShareAll ShareMode = "all"
	// ShareNone shares no session.
	ShareNone ShareMode = "none"
	// ShareSelect shares exactly the selected sessions.
	ShareSelect ShareMode = "select"
	// ShareLater means the operator has not decided yet ("configure later
	// from the Hub"): nothing is shared until the mode changes.
	ShareLater ShareMode = "later"
)

// ErrInvalidShare is returned (wrapped) by [Store.SaveShare] for an unknown
// mode or an empty session name. Test with [errors.Is].
var ErrInvalidShare = errors.New("store: invalid share selection")

// Valid reports whether m is one of the four modes.
func (m ShareMode) Valid() bool {
	switch m {
	case ShareAll, ShareNone, ShareSelect, ShareLater:
		return true
	}
	return false
}

// ShareSelection is the saved share choice.
type ShareSelection struct {
	// Mode is the share mode. [Store.LoadShare] returns ShareLater when
	// nothing has been saved yet.
	Mode ShareMode
	// Sessions are the selected tmux session names, in the order given and
	// without duplicates. They are kept for every mode (so switching from
	// select to none and back keeps the list) but only ShareSelect uses them.
	Sessions []string
	// Configured is false until the first successful [Store.SaveShare]. It
	// is ignored by SaveShare.
	Configured bool
}

// Includes reports whether the selection shares the named session: always for
// ShareAll, never for ShareNone and ShareLater, and membership for
// ShareSelect. It is the pure rule behind [Store.IsShared].
func (sel ShareSelection) Includes(session string) bool {
	switch sel.Mode {
	case ShareAll:
		return true
	case ShareSelect:
		for _, s := range sel.Sessions {
			if s == session {
				return true
			}
		}
	}
	return false
}

// ShareState is the saved share choice together with its revision: the number
// of committed changes of the choice since the database was created, plus one. The
// revision only rises (a pairing reset that removes the choice counts as a
// change), so two states of one Worker are ordered by it: the Hub uses it to
// tell a late answer from a current one.
type ShareState struct {
	ShareSelection
	// Revision is the revision of this choice: 1 for a database that never
	// saw a change, and it rises with every change. 0 does not occur: it is
	// what a Worker without revisions reports.
	Revision uint64
}

// SaveShare replaces the saved share choice in one transaction. Duplicate
// names are dropped (the first occurrence keeps its place).
func (s *Store) SaveShare(sel ShareSelection) error {
	return s.SaveShareContext(context.Background(), sel)
}

// SaveShareContext is [Store.SaveShare] with a context.
func (s *Store) SaveShareContext(ctx context.Context, sel ShareSelection) error {
	_, err := s.CommitShareContext(ctx, sel)
	return err
}

// CommitShareContext is [Store.SaveShareContext] that also returns what it
// committed, with the new revision, so that a caller can report the state it
// has just made durable without reading the database again (a read after the
// commit can fail; the commit stands).
func (s *Store) CommitShareContext(ctx context.Context, sel ShareSelection) (ShareState, error) {
	if !sel.Mode.Valid() {
		return ShareState{}, fmt.Errorf("%w: unknown mode %q (want all, none, select or later)", ErrInvalidShare, sel.Mode)
	}
	names := make([]string, 0, len(sel.Sessions))
	seen := make(map[string]bool, len(sel.Sessions))
	for _, n := range sel.Sessions {
		if n == "" {
			return ShareState{}, fmt.Errorf("%w: empty session name", ErrInvalidShare)
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	now := s.clock.now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ShareState{}, fmt.Errorf("store: save share selection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO share_config (id, mode, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`,
		string(sel.Mode), now); err != nil {
		return ShareState{}, fmt.Errorf("store: save share selection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM share_selected`); err != nil {
		return ShareState{}, fmt.Errorf("store: save share selection: %w", err)
	}
	for i, n := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO share_selected (session_name, position) VALUES (?, ?)`, n, i); err != nil {
			return ShareState{}, fmt.Errorf("store: save share selection: %w", err)
		}
	}
	rev, err := bumpShareRevision(ctx, tx)
	if err != nil {
		return ShareState{}, err
	}
	if err := tx.Commit(); err != nil {
		return ShareState{}, fmt.Errorf("store: save share selection: %w", err)
	}
	return ShareState{ShareSelection: ShareSelection{Mode: sel.Mode, Sessions: names, Configured: true}, Revision: rev}, nil
}

// bumpShareRevision raises the share revision by one inside tx and returns it.
func bumpShareRevision(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var rev int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE share_revision SET revision = revision + 1 WHERE id = 1 RETURNING revision`).Scan(&rev); err != nil {
		return 0, fmt.Errorf("store: raise the share revision: %w", err)
	}
	return uint64(rev), nil
}

// LoadShare returns the saved share choice. Before the first SaveShare it
// returns {Mode: ShareLater, Configured: false}.
func (s *Store) LoadShare() (ShareSelection, error) {
	return s.LoadShareContext(context.Background())
}

// LoadShareContext is [Store.LoadShare] with a context.
func (s *Store) LoadShareContext(ctx context.Context) (ShareSelection, error) {
	st, err := s.LoadShareStateContext(ctx)
	return st.ShareSelection, err
}

// LoadShareStateContext returns the saved share choice with its revision, read
// in one statement, so that mode, list and revision are one snapshot (and the
// read does not take the write lock that an immediate transaction would).
func (s *Store) LoadShareStateContext(ctx context.Context) (ShareState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.mode, sel.session_name, r.revision
		 FROM (SELECT 1) AS one
		 LEFT JOIN share_config AS c ON c.id = 1
		 LEFT JOIN share_revision AS r ON r.id = 1
		 LEFT JOIN share_selected AS sel
		 ORDER BY sel.position`)
	if err != nil {
		return ShareState{}, fmt.Errorf("store: load share selection: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := ShareState{ShareSelection: ShareSelection{Mode: ShareLater, Sessions: []string{}}}
	for rows.Next() {
		var mode, name sql.NullString
		var rev sql.NullInt64
		if err := rows.Scan(&mode, &name, &rev); err != nil {
			return ShareState{}, fmt.Errorf("store: load share selection: %w", err)
		}
		if mode.Valid {
			out.Mode, out.Configured = ShareMode(mode.String), true
		}
		if name.Valid {
			out.Sessions = append(out.Sessions, name.String)
		}
		if rev.Valid && rev.Int64 > 0 {
			out.Revision = uint64(rev.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return ShareState{}, fmt.Errorf("store: load share selection: %w", err)
	}
	return out, nil
}

// IsShared reports whether the named session is shared with the Hub: true for
// mode all (also for sessions created after the choice was saved), false for
// none and later, and membership in the saved list for select. Nothing saved
// yet counts as later: not shared.
func (s *Store) IsShared(session string) (bool, error) {
	return s.IsSharedContext(context.Background(), session)
}

// IsSharedContext is [Store.IsShared] with a context.
func (s *Store) IsSharedContext(ctx context.Context, session string) (bool, error) {
	var shared bool
	err := s.db.QueryRowContext(ctx,
		`SELECT CASE (SELECT mode FROM share_config WHERE id = 1)
		     WHEN 'all' THEN 1
		     WHEN 'select' THEN EXISTS (SELECT 1 FROM share_selected WHERE session_name = ?)
		     ELSE 0
		 END`, session).Scan(&shared)
	if err != nil {
		return false, fmt.Errorf("store: is shared: %w", err)
	}
	return shared, nil
}
