package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SessionControlRow is one session-control grant: the controller session
// (WorkerID, SessionName) may reach the targets in TargetsJSON. The store
// keeps TargetsJSON as given; the ops layer validates its shape (the resources
// of an API-key scope).
type SessionControlRow struct {
	WorkerID    string
	SessionName string
	TargetsJSON string
	CreatedBy   string
	CreatedAt   int64 // unix milliseconds
}

const sessionControlColumns = `worker_id, session_name, targets_json, created_by, created_at`

func scanSessionControl(sc interface{ Scan(...any) error }) (SessionControlRow, error) {
	var r SessionControlRow
	err := sc.Scan(&r.WorkerID, &r.SessionName, &r.TargetsJSON, &r.CreatedBy, &r.CreatedAt)
	return r, err
}

// ListSessionControl returns every grant, ordered by Worker and session.
func (s *Store) ListSessionControl(ctx context.Context) ([]SessionControlRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionControlColumns+` FROM session_control ORDER BY worker_id, session_name`)
	if err != nil {
		return nil, fmt.Errorf("store: list session control: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionControlRow
	for rows.Next() {
		r, err := scanSessionControl(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list session control: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list session control: %w", err)
	}
	return out, nil
}

// SessionControl returns the grant of one controller session, or
// [ErrNotFound]. It reads the table on every call (no cache).
func (s *Store) SessionControl(ctx context.Context, workerID, sessionName string) (SessionControlRow, error) {
	r, err := scanSessionControl(s.db.QueryRowContext(ctx,
		`SELECT `+sessionControlColumns+` FROM session_control WHERE worker_id = ? AND session_name = ?`,
		workerID, sessionName))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionControlRow{}, ErrNotFound
	case err != nil:
		return SessionControlRow{}, fmt.Errorf("store: read session control: %w", err)
	}
	return r, nil
}

// PutSessionControl stores the grant of a controller session, replacing the
// targets, creator and creation time of an existing one.
func (s *Store) PutSessionControl(ctx context.Context, r SessionControlRow) error {
	if r.WorkerID == "" || r.SessionName == "" || r.TargetsJSON == "" {
		return errors.New("store: a session-control row needs a Worker, a session and targets")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_control (`+sessionControlColumns+`) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (worker_id, session_name) DO UPDATE SET
		   targets_json = excluded.targets_json, created_by = excluded.created_by, created_at = excluded.created_at`,
		r.WorkerID, r.SessionName, r.TargetsJSON, r.CreatedBy, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: put session control: %w", err)
	}
	return nil
}

// DeleteSessionControl removes the grant of one controller session and
// reports whether it existed.
func (s *Store) DeleteSessionControl(ctx context.Context, workerID, sessionName string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM session_control WHERE worker_id = ? AND session_name = ?`, workerID, sessionName)
	if err != nil {
		return false, fmt.Errorf("store: delete session control: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete session control: %w", err)
	}
	return n > 0, nil
}

// DeleteSessionControlOfWorker deletes every grant whose controller session
// lives on the Worker, on ex: the open transaction of a reassignment or a
// removal, next to [Store.DeleteTrustOfWorker]. Grants that name the Worker
// as a target are left alone; the stale id matches nothing. It returns the
// number of rows deleted.
func (s *Store) DeleteSessionControlOfWorker(ctx context.Context, ex Execer, workerID string) (int64, error) {
	if ex == nil {
		ex = s.db
	}
	res, err := ex.ExecContext(ctx, `DELETE FROM session_control WHERE worker_id = ?`, workerID)
	if err != nil {
		return 0, fmt.Errorf("store: delete session control of worker: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete session control of worker: %w", err)
	}
	return n, nil
}
