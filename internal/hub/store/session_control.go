package store

// Session control (v0.8) on top of the minion-control tables (v0.9.0, A17).
// A controller session's grant is no longer stored as such: it is computed
// from its master-session nomination and the Workers that accept control from
// its Worker (minion_control.go). The functions keep their v0.8 signatures, so
// the call path (CallServer.lookupGrant), authorization and origins are
// unchanged; only where the targets come from is new.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SessionControlRow is one session-control grant: the controller session
// (WorkerID, SessionName) may reach the targets in TargetsJSON, a JSON array
// in the shape of the resources of an API-key scope.
type SessionControlRow struct {
	WorkerID    string
	SessionName string
	TargetsJSON string
	CreatedBy   string
	CreatedAt   int64 // unix milliseconds
	// SessionTargets are the targets of TargetsJSON that name one session,
	// each with the time it was named (the acceptance that lists it, or the
	// nomination for the limit of a migrated grant). The call path fences
	// them like the target of a direct grant: a session first seen after it
	// was named is another terminal.
	SessionTargets []ControlSessionTarget
}

// ControlSessionTarget is one session target of a session-control grant.
type ControlSessionTarget struct {
	WorkerID    string
	SessionName string
	NamedAt     int64 // unix milliseconds
}

// TargetsJSONWithout returns TargetsJSON without the session targets that
// drop reports, and "" when no target is left. A row without session targets
// is returned unchanged.
func (r SessionControlRow) TargetsJSONWithout(drop func(ControlSessionTarget) bool) string {
	if len(r.SessionTargets) == 0 {
		return r.TargetsJSON
	}
	gone := map[[2]string]bool{}
	for _, t := range r.SessionTargets {
		if drop(t) {
			gone[[2]string{t.WorkerID, t.SessionName}] = true
		}
	}
	if len(gone) == 0 {
		return r.TargetsJSON
	}
	var all []controlResource
	if err := json.Unmarshal([]byte(r.TargetsJSON), &all); err != nil {
		return "" // an unreadable list allows nothing
	}
	kept := all[:0]
	for _, c := range all {
		if c.Kind == "session" && gone[[2]string{c.WorkerID, c.SessionName}] {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return ""
	}
	return targetsJSON(kept)
}

// ListSessionControl returns the computed grant of every master session that
// reaches at least one target, ordered by Worker and session.
func (s *Store) ListSessionControl(ctx context.Context) ([]SessionControlRow, error) {
	masters, err := s.ListMasterSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list session control: %w", err)
	}
	var out []SessionControlRow
	for _, m := range masters {
		r, err := s.SessionControl(ctx, m.WorkerID, m.SessionName)
		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// SessionControl returns the computed grant of one controller session, or
// [ErrNotFound] when the session is no master session or no Worker accepts
// control from its Worker. It reads the tables on every call (no cache).
func (s *Store) SessionControl(ctx context.Context, workerID, sessionName string) (SessionControlRow, error) {
	m, err := s.MasterSession(ctx, workerID, sessionName)
	if err != nil {
		return SessionControlRow{}, err
	}
	targets, err := s.controlTargets(ctx, workerID, m.LegacyTargetsJSON)
	if err != nil {
		return SessionControlRow{}, err
	}
	if len(targets) == 0 {
		return SessionControlRow{}, ErrNotFound
	}
	named, err := s.sessionTargetsNamed(ctx, workerID, targets, m.CreatedAt)
	if err != nil {
		return SessionControlRow{}, err
	}
	return SessionControlRow{WorkerID: workerID, SessionName: sessionName, TargetsJSON: targetsJSON(targets),
		CreatedBy: m.CreatedBy, CreatedAt: m.CreatedAt, SessionTargets: named}, nil
}

// sessionTargetsNamed returns the session targets of source with the time
// each was named: the creation of the acceptance that lists it, else (the
// limit of a migrated grant) nominated, the creation of the nomination.
func (s *Store) sessionTargetsNamed(ctx context.Context, source string, targets []controlResource, nominated int64) ([]ControlSessionTarget, error) {
	var out []ControlSessionTarget
	accepts := map[string]struct {
		names map[string]bool
		at    int64
	}{}
	loaded := false
	for _, t := range targets {
		if t.Kind != "session" {
			continue
		}
		if !loaded {
			loaded = true
			rows, err := s.db.QueryContext(ctx, `SELECT target_worker_id, sessions_json, created_at FROM control_accept WHERE source_worker_id = ?`, source)
			if err != nil {
				return nil, fmt.Errorf("store: read acceptances: %w", err)
			}
			for rows.Next() {
				var (
					target string
					sess   sql.NullString
					at     int64
				)
				if err := rows.Scan(&target, &sess, &at); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("store: read acceptances: %w", err)
				}
				var names []string
				if sess.Valid {
					_ = json.Unmarshal([]byte(sess.String), &names)
				}
				set := map[string]bool{}
				for _, n := range names {
					set[n] = true
				}
				accepts[target] = struct {
					names map[string]bool
					at    int64
				}{set, at}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("store: read acceptances: %w", err)
			}
		}
		at := nominated
		if a, ok := accepts[t.WorkerID]; ok && a.names[t.SessionName] {
			at = a.at
		}
		out = append(out, ControlSessionTarget{WorkerID: t.WorkerID, SessionName: t.SessionName, NamedAt: at})
	}
	return out, nil
}

// PutSessionControl is the v0.8 grant written in the new model: the session
// becomes a master session limited to targetsJSON, and every target Worker
// accepts control from its Worker. It does not check the Worker; the
// operation uses [Store.PutSessionControlIfOwner].
func (s *Store) PutSessionControl(ctx context.Context, r SessionControlRow) error {
	if r.WorkerID == "" || r.SessionName == "" || r.TargetsJSON == "" {
		return errors.New("store: a session-control row needs a Worker, a session and targets")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error { return putLegacyGrant(ctx, tx, r) })
}

// PutSessionControlIfOwner is [Store.PutSessionControl] made conditional on
// the owner of the controller Worker, in one transaction: a removal or
// reassignment deletes the Worker's rows in its own transaction, so a grant
// decided before it can never be stored after it ([ErrWorkerOwnerChanged],
// [ErrWorkerNotFound]).
func (s *Store) PutSessionControlIfOwner(ctx context.Context, r SessionControlRow, expectedOwnerID string) error {
	if r.WorkerID == "" || r.SessionName == "" || r.TargetsJSON == "" || expectedOwnerID == "" {
		return errors.New("store: a session-control row needs a Worker, a session, targets and an owner")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var owner string
		if err := rowErr("read worker owner", tx.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ?`, r.WorkerID).Scan(&owner)); err != nil {
			return err
		}
		if owner != expectedOwnerID {
			return ErrWorkerOwnerChanged
		}
		return putLegacyGrant(ctx, tx, r)
	})
}

// putLegacyGrant nominates the master session with its exact targets as limit
// and lets each target Worker (all Workers for everything) accept control from
// the controller's Worker, as migration 0025 does.
func putLegacyGrant(ctx context.Context, tx *sql.Tx, r SessionControlRow) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO master_session (worker_id, session_name, legacy_targets_json, created_by, created_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (worker_id, session_name) DO UPDATE SET
		   legacy_targets_json = excluded.legacy_targets_json, created_by = excluded.created_by, created_at = excluded.created_at`,
		r.WorkerID, r.SessionName, r.TargetsJSON, r.CreatedBy, r.CreatedAt); err != nil {
		return fmt.Errorf("store: put master session: %w", masterSlotError(err))
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at)
		 SELECT json_extract(t.value, '$.worker_id'), ?, NULL, ?, ?
		 FROM json_each(?) t
		 WHERE json_extract(t.value, '$.kind') IN ('worker', 'session')
		   AND json_extract(t.value, '$.worker_id') IS NOT NULL AND json_extract(t.value, '$.worker_id') <> ?`,
		r.WorkerID, r.CreatedBy, r.CreatedAt, r.TargetsJSON, r.WorkerID); err != nil {
		return fmt.Errorf("store: put acceptances: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at)
		 SELECT w.id, ?, NULL, ?, ? FROM workers w
		 WHERE w.id <> ? AND EXISTS (SELECT 1 FROM json_each(?) t WHERE json_extract(t.value, '$.kind') = 'everything')`,
		r.WorkerID, r.CreatedBy, r.CreatedAt, r.WorkerID, r.TargetsJSON); err != nil {
		return fmt.Errorf("store: put acceptances: %w", err)
	}
	return nil
}

// DeleteSessionControl ends the master nomination of one session and reports
// whether it existed. Acceptances stay: they belong to the target Workers.
func (s *Store) DeleteSessionControl(ctx context.Context, workerID, sessionName string) (bool, error) {
	return s.DeleteMasterSession(ctx, workerID, sessionName)
}

// DeleteSessionControlOfWorker deletes every minion-control row of the Worker
// on ex: its master sessions, its control settings, and the acceptances it
// gave or received. It runs in the open transaction of a reassignment or a
// removal, next to [Store.DeleteTrustOfWorker], so no control decided for the
// old owner survives. It returns the number of rows deleted.
func (s *Store) DeleteSessionControlOfWorker(ctx context.Context, ex Execer, workerID string) (int64, error) {
	if ex == nil {
		ex = s.db
	}
	var total int64
	for _, q := range []string{
		`DELETE FROM master_session WHERE worker_id = ?1`,
		`DELETE FROM worker_control WHERE worker_id = ?1`,
		`DELETE FROM control_accept WHERE target_worker_id = ?1 OR source_worker_id = ?1`,
		`DELETE FROM session_control WHERE worker_id = ?1`,
	} {
		res, err := ex.ExecContext(ctx, q, workerID)
		if err != nil {
			return total, fmt.Errorf("store: delete minion control of worker: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: delete minion control of worker: %w", err)
		}
		total += n
	}
	return total, nil
}
