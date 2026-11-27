package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// keptSessionsSchema is the keep list of the Worker (session_keep_set,
// v0.24.0): the sessions it recreates when it starts again, each with its last
// saved layout. Like op_result_callers ([resultCallersSchema]) it is a table of
// its own outside the numbered migrations: a release that does not know it
// ignores it, the schema version stays the one of the release before, and a
// self-update that adds it writes no pre-migration backup. A rollback of the
// update therefore keeps the table and the keep choices made under the new
// binary, and an update again finds them. Every statement is idempotent; a
// later numbered migration may adopt the table with the same IF NOT EXISTS
// form.
//
// layout is the JSON the keeper saved (windows, panes, folders, tools and
// conversation ids; never keys, input or environment values), "" until the
// first save. instance_id is the terminal identity of the session when it was
// last seen, so a rename of the same terminal session moves the row.
const keptSessionsSchema = `
CREATE TABLE IF NOT EXISTS kept_sessions (
    name        TEXT    PRIMARY KEY CHECK (length(name) > 0),
    instance_id TEXT    NOT NULL DEFAULT '',
    layout      TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    saved_at    INTEGER NOT NULL DEFAULT 0
);`

// MaxKeptLayout bounds the saved layout of one session in bytes.
const MaxKeptLayout = 256 << 10

// MaxKeptSessions bounds the keep list.
const MaxKeptSessions = 256

// ErrKeepListFull is returned by [Store.KeepSession] when the keep list holds
// [MaxKeptSessions] sessions already.
var ErrKeepListFull = errors.New("store: the keep list is full")

// KeptSession is one row of the keep list.
type KeptSession struct {
	Name       string
	InstanceID string
	// Layout is the saved layout JSON; "" until the first save.
	Layout  string
	Created time.Time
	// Saved is when the layout was last written; zero until the first save.
	Saved time.Time
}

// ensureKeptSessions creates kept_sessions ([keptSessionsSchema]) in a Worker
// database (one that has op_results); any other database is left alone.
func ensureKeptSessions(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'op_results'`).Scan(&n); err != nil {
		return fmt.Errorf("store: keep list: %w", err)
	}
	if n == 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, keptSessionsSchema); err != nil {
		return fmt.Errorf("store: keep list: %w", err)
	}
	return nil
}

// KeepSession adds name to the keep list with the terminal instance id it has
// now. A session already kept keeps its saved layout; its instance id is
// updated when instanceID is not empty.
func (s *Store) KeepSession(ctx context.Context, name, instanceID string) error {
	if name == "" {
		return errors.New("store: keep a session without a name")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists, total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kept_sessions WHERE name = ?`, name).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kept_sessions`).Scan(&total); err != nil {
			return err
		}
		if total >= MaxKeptSessions {
			return ErrKeepListFull
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kept_sessions (name, instance_id, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET
		   instance_id = CASE WHEN excluded.instance_id = '' THEN instance_id ELSE excluded.instance_id END`,
		name, instanceID, s.clock.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// UnkeepSession removes name and its saved layout from the keep list. A name
// that is not kept is no error; removed reports whether a row was deleted.
func (s *Store) UnkeepSession(ctx context.Context, name string) (removed bool, err error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM kept_sessions WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// KeptSessions returns the keep list, sorted by name.
func (s *Store) KeptSessions(ctx context.Context) ([]KeptSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, instance_id, layout, created_at, saved_at FROM kept_sessions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []KeptSession
	for rows.Next() {
		var k KeptSession
		var created, saved int64
		if err := rows.Scan(&k.Name, &k.InstanceID, &k.Layout, &created, &saved); err != nil {
			return nil, err
		}
		k.Created = time.UnixMilli(created)
		if saved > 0 {
			k.Saved = time.UnixMilli(saved)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeptNames returns the names on the keep list as a set.
func (s *Store) KeptNames(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM kept_sessions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// SaveKeptLayout stores the layout and instance id of a kept session. A
// session that is no longer kept (unkept meanwhile) is not added again:
// saved reports whether a row was written.
func (s *Store) SaveKeptLayout(ctx context.Context, name, instanceID, layout string) (saved bool, err error) {
	if len(layout) > MaxKeptLayout {
		return false, fmt.Errorf("store: the layout of %q has %d bytes, more than %d", name, len(layout), MaxKeptLayout)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE kept_sessions SET layout = ?, saved_at = ?,
		   instance_id = CASE WHEN ? = '' THEN instance_id ELSE ? END
		 WHERE name = ?`,
		layout, s.clock.now().UnixMilli(), instanceID, instanceID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RenameKeptSession moves the keep row of from to the name to (a rename of
// the same terminal session). It does nothing when from is not kept or to is
// kept already (the existing row of to wins); moved reports whether it moved.
func (s *Store) RenameKeptSession(ctx context.Context, from, to string) (moved bool, err error) {
	if from == "" || to == "" || from == to {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE kept_sessions SET name = ? WHERE name = ?
		   AND NOT EXISTS (SELECT 1 FROM kept_sessions WHERE name = ?)`, to, from, to)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
