package store

// Minion control (v0.9.0, A17): master sessions, the Workers that accept
// control from other Workers, and the user-managed switch. See migration
// 0025_minion_control.sql for the model.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// MasterRow is one nominated master session.
type MasterRow struct {
	WorkerID    string
	SessionName string
	// LegacyTargetsJSON is the exact target list of a migrated v0.8 grant (an
	// extra limit); empty for a nomination made in v0.9.0 or later.
	LegacyTargetsJSON string
	CreatedBy         string
	CreatedAt         int64
}

// AcceptRow says that TargetWorkerID accepts control from SourceWorkerID's
// master sessions; Sessions limits it to some of the target's sessions (nil:
// all its shared sessions).
type AcceptRow struct {
	TargetWorkerID string
	SourceWorkerID string
	Sessions       []string
	CreatedBy      string
	CreatedAt      int64
}

// WorkerControlRow is the control setting of one Worker; a Worker without a
// row has the defaults (user-managed on, accepts nobody).
type WorkerControlRow struct {
	WorkerID    string
	UserManaged bool
	AcceptAll   bool
}

// ListMasterSessions returns every nomination, ordered by Worker and session.
func (s *Store) ListMasterSessions(ctx context.Context) ([]MasterRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT worker_id, session_name, COALESCE(legacy_targets_json, ''), created_by, created_at
		 FROM master_session ORDER BY worker_id, session_name`)
	if err != nil {
		return nil, fmt.Errorf("store: list master sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MasterRow
	for rows.Next() {
		var m MasterRow
		if err := rows.Scan(&m.WorkerID, &m.SessionName, &m.LegacyTargetsJSON, &m.CreatedBy, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list master sessions: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MasterSession returns one nomination or [ErrNotFound].
func (s *Store) MasterSession(ctx context.Context, workerID, session string) (MasterRow, error) {
	m := MasterRow{WorkerID: workerID, SessionName: session}
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(legacy_targets_json, ''), created_by, created_at FROM master_session WHERE worker_id = ? AND session_name = ?`,
		workerID, session).Scan(&m.LegacyTargetsJSON, &m.CreatedBy, &m.CreatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return MasterRow{}, ErrNotFound
	case err != nil:
		return MasterRow{}, fmt.Errorf("store: read master session: %w", err)
	}
	return m, nil
}

// PutMasterSession nominates a master session. A nomination made here has no
// legacy limit (it clears the one a migrated grant had).
func (s *Store) PutMasterSession(ctx context.Context, m MasterRow) error {
	if m.WorkerID == "" || m.SessionName == "" {
		return errors.New("store: a master session needs a Worker and a session")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO master_session (worker_id, session_name, legacy_targets_json, created_by, created_at) VALUES (?, ?, NULL, ?, ?)
		 ON CONFLICT (worker_id, session_name) DO UPDATE SET legacy_targets_json = NULL, created_by = excluded.created_by, created_at = excluded.created_at`,
		m.WorkerID, m.SessionName, m.CreatedBy, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: put master session: %w", err)
	}
	return nil
}

// DeleteMasterSession ends a nomination and reports whether it existed.
func (s *Store) DeleteMasterSession(ctx context.Context, workerID, session string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM master_session WHERE worker_id = ? AND session_name = ?`, workerID, session)
	if err != nil {
		return false, fmt.Errorf("store: delete master session: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListAccepts returns every acceptance, ordered by target and source.
func (s *Store) ListAccepts(ctx context.Context) ([]AcceptRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT target_worker_id, source_worker_id, sessions_json, created_by, created_at
		 FROM control_accept ORDER BY target_worker_id, source_worker_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list acceptances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AcceptRow
	for rows.Next() {
		var a AcceptRow
		var sess sql.NullString
		if err := rows.Scan(&a.TargetWorkerID, &a.SourceWorkerID, &sess, &a.CreatedBy, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list acceptances: %w", err)
		}
		if sess.Valid {
			_ = json.Unmarshal([]byte(sess.String), &a.Sessions)
			if a.Sessions == nil {
				a.Sessions = []string{}
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PutAccept stores an acceptance (replacing its session limit).
func (s *Store) PutAccept(ctx context.Context, a AcceptRow) error {
	if a.TargetWorkerID == "" || a.SourceWorkerID == "" || a.TargetWorkerID == a.SourceWorkerID {
		return errors.New("store: an acceptance needs two different Workers")
	}
	var sess any
	if a.Sessions != nil {
		b, err := json.Marshal(a.Sessions)
		if err != nil {
			return err
		}
		sess = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (target_worker_id, source_worker_id) DO UPDATE SET sessions_json = excluded.sessions_json, created_by = excluded.created_by, created_at = excluded.created_at`,
		a.TargetWorkerID, a.SourceWorkerID, sess, a.CreatedBy, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: put acceptance: %w", err)
	}
	return nil
}

// DeleteAccept removes an acceptance and reports whether it existed.
func (s *Store) DeleteAccept(ctx context.Context, target, source string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM control_accept WHERE target_worker_id = ? AND source_worker_id = ?`, target, source)
	if err != nil {
		return false, fmt.Errorf("store: delete acceptance: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// WorkerControl returns the control setting of a Worker (defaults when none).
func (s *Store) WorkerControl(ctx context.Context, workerID string) (WorkerControlRow, error) {
	r := WorkerControlRow{WorkerID: workerID, UserManaged: true}
	var um, all int
	err := s.db.QueryRowContext(ctx, `SELECT user_managed, accept_all FROM worker_control WHERE worker_id = ?`, workerID).Scan(&um, &all)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return r, nil
	case err != nil:
		return r, fmt.Errorf("store: read worker control: %w", err)
	}
	r.UserManaged, r.AcceptAll = um == 1, all == 1
	return r, nil
}

// ListWorkerControl returns the Workers whose setting differs from the defaults.
func (s *Store) ListWorkerControl(ctx context.Context) (map[string]WorkerControlRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker_id, user_managed, accept_all FROM worker_control`)
	if err != nil {
		return nil, fmt.Errorf("store: list worker control: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]WorkerControlRow{}
	for rows.Next() {
		var r WorkerControlRow
		var um, all int
		if err := rows.Scan(&r.WorkerID, &um, &all); err != nil {
			return nil, err
		}
		r.UserManaged, r.AcceptAll = um == 1, all == 1
		out[r.WorkerID] = r
	}
	return out, rows.Err()
}

// SetWorkerControl changes the user-managed and accept-all switches of a
// Worker; nil leaves one as it is. One statement updates only the given
// columns, so two concurrent changes of different switches both hold.
func (s *Store) SetWorkerControl(ctx context.Context, workerID string, userManaged, acceptAll *bool) error {
	return setWorkerControl(ctx, s.db, workerID, userManaged, acceptAll)
}

func optInt(b *bool) any {
	if b == nil {
		return nil
	}
	return boolInt(*b)
}

func setWorkerControl(ctx context.Context, ex Execer, workerID string, userManaged, acceptAll *bool) error {
	um, all := optInt(userManaged), optInt(acceptAll)
	_, err := ex.ExecContext(ctx,
		`INSERT INTO worker_control (worker_id, user_managed, accept_all) VALUES (?1, COALESCE(?2, 1), COALESCE(?3, 0))
		 ON CONFLICT (worker_id) DO UPDATE SET user_managed = COALESCE(?2, user_managed), accept_all = COALESCE(?3, accept_all)`,
		workerID, um, all)
	if err != nil {
		return fmt.Errorf("store: set worker control: %w", err)
	}
	return nil
}

// checkOwners verifies, inside tx, that each Worker still has the owner the
// change was decided for: a removal or reassignment in between (which deletes
// the Worker's rows) must not be undone by a change in flight.
func checkOwners(ctx context.Context, tx *sql.Tx, owners map[string]string) error {
	for id, want := range owners {
		var owner string
		if err := rowErr("read worker owner", tx.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ?`, id).Scan(&owner)); err != nil {
			return err
		}
		if owner != want {
			return ErrWorkerOwnerChanged
		}
	}
	return nil
}

// SetWorkerControlIfOwner is [Store.SetWorkerControl] while the Worker still
// has owner.
func (s *Store) SetWorkerControlIfOwner(ctx context.Context, workerID, owner string, userManaged, acceptAll *bool) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkOwners(ctx, tx, map[string]string{workerID: owner}); err != nil {
			return err
		}
		return setWorkerControl(ctx, tx, workerID, userManaged, acceptAll)
	})
}

// PutMasterSessionIfOwner is [Store.PutMasterSession] while the Worker still
// has owner. It keeps a migrated legacy limit unless clearLegacy.
func (s *Store) PutMasterSessionIfOwner(ctx context.Context, m MasterRow, owner string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkOwners(ctx, tx, map[string]string{m.WorkerID: owner}); err != nil {
			return err
		}
		var legacy any
		if m.LegacyTargetsJSON != "" {
			legacy = m.LegacyTargetsJSON
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO master_session (worker_id, session_name, legacy_targets_json, created_by, created_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (worker_id, session_name) DO UPDATE SET legacy_targets_json = excluded.legacy_targets_json, created_by = excluded.created_by, created_at = excluded.created_at`,
			m.WorkerID, m.SessionName, legacy, m.CreatedBy, m.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: put master session: %w", err)
		}
		return nil
	})
}

// PutAcceptIfOwners is [Store.PutAccept] while both Workers still have the
// owners the change was decided for.
func (s *Store) PutAcceptIfOwners(ctx context.Context, a AcceptRow, targetOwner, sourceOwner string) error {
	if a.TargetWorkerID == "" || a.SourceWorkerID == "" || a.TargetWorkerID == a.SourceWorkerID {
		return errors.New("store: an acceptance needs two different Workers")
	}
	var sess any
	if a.Sessions != nil {
		b, err := json.Marshal(a.Sessions)
		if err != nil {
			return err
		}
		sess = string(b)
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkOwners(ctx, tx, map[string]string{a.TargetWorkerID: targetOwner, a.SourceWorkerID: sourceOwner}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO control_accept (target_worker_id, source_worker_id, sessions_json, created_by, created_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (target_worker_id, source_worker_id) DO UPDATE SET sessions_json = excluded.sessions_json, created_by = excluded.created_by, created_at = excluded.created_at`,
			a.TargetWorkerID, a.SourceWorkerID, sess, a.CreatedBy, a.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: put acceptance: %w", err)
		}
		return nil
	})
}

// controlResource is one target of a computed grant, in the JSON shape of an
// API-key scope resource (ops.KeyResource).
type controlResource struct {
	Kind        string `json:"kind"`
	WorkerID    string `json:"worker_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
}

// controlTargets computes what a master session on source reaches: every
// other Worker that accepts control from source (or from all Workers),
// narrowed to the accepted sessions and, for a migrated grant, to its legacy
// targets. The owner limit and the target's share rule are applied later by
// authz and the target Worker, as for any grant.
func (s *Store) controlTargets(ctx context.Context, source, legacy string) ([]controlResource, error) {
	type accept struct {
		target   string
		sessions *string
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT target_worker_id, sessions_json FROM control_accept WHERE source_worker_id = ?1 AND target_worker_id <> ?1
		 UNION
		 SELECT worker_id, NULL FROM worker_control WHERE accept_all = 1 AND worker_id <> ?1
		   AND worker_id NOT IN (SELECT target_worker_id FROM control_accept WHERE source_worker_id = ?1)`, source)
	if err != nil {
		return nil, fmt.Errorf("store: compute control targets: %w", err)
	}
	var acc []accept
	for rows.Next() {
		var a accept
		var sess sql.NullString
		if err := rows.Scan(&a.target, &sess); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if sess.Valid {
			v := sess.String
			a.sessions = &v
		}
		acc = append(acc, a)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []controlResource
	for _, a := range acc {
		if a.sessions == nil {
			out = append(out, controlResource{Kind: "worker", WorkerID: a.target})
			continue
		}
		var names []string
		_ = json.Unmarshal([]byte(*a.sessions), &names)
		for _, n := range names {
			out = append(out, controlResource{Kind: "session", WorkerID: a.target, SessionName: n})
		}
	}
	if legacy != "" {
		out = intersectLegacy(out, legacy)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkerID != out[j].WorkerID {
			return out[i].WorkerID < out[j].WorkerID
		}
		return out[i].SessionName < out[j].SessionName
	})
	return out, nil
}

// intersectLegacy narrows computed targets to the exact targets of a migrated
// v0.8 grant: nothing a v0.8 grant did not reach becomes reachable.
func intersectLegacy(computed []controlResource, legacy string) []controlResource {
	var lim []controlResource
	if err := json.Unmarshal([]byte(legacy), &lim); err != nil {
		return nil // an unreadable limit allows nothing
	}
	everything := false
	whole := map[string]bool{}
	sessions := map[string][]string{}
	for _, l := range lim {
		switch l.Kind {
		case "everything":
			everything = true
		case "worker":
			whole[l.WorkerID] = true
		case "session":
			sessions[l.WorkerID] = append(sessions[l.WorkerID], l.SessionName)
		}
	}
	var out []controlResource
	for _, c := range computed {
		switch {
		case everything || whole[c.WorkerID]:
			out = append(out, c)
		case c.Kind == "worker":
			for _, n := range sessions[c.WorkerID] {
				out = append(out, controlResource{Kind: "session", WorkerID: c.WorkerID, SessionName: n})
			}
		default:
			for _, n := range sessions[c.WorkerID] {
				if n == c.SessionName {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

func targetsJSON(t []controlResource) string {
	b, _ := json.Marshal(t)
	return string(b)
}
