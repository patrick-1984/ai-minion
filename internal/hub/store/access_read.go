package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// AccessUser is what the access read model needs of one account.
type AccessUser struct {
	Username string
	// Active: the account is active and holds a role, so its Workers may
	// make calls. Admin: it is also an administrator.
	Active, Admin bool
}

// AccessSnapshot is every durable fact the access matrix and the explainer
// are computed from, read at one moment.
type AccessSnapshot struct {
	Workers []WorkerRow
	Users   map[string]AccessUser
	Masters []MasterRow
	Bound   []BoundMasterRow
	Accepts []AcceptRow
	Control map[string]WorkerControlRow
	Shares  map[string]ops.ShareDecl
	// Pending are the restrictive share changes that are dispatched and not
	// settled: a session one of them excludes does not count as shared.
	Pending []PendingShare
	Grants  []SessionGrantRow
}

// AccessSnapshot reads the access facts in one read-only transaction: every
// table is seen at the same commit, and the transaction never takes the write
// lock (it is a deferred BEGIN, unlike [Store.Tx]).
func (s *Store) AccessSnapshot(ctx context.Context) (AccessSnapshot, error) {
	var out AccessSnapshot
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, fmt.Errorf("store: access snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := readAccessSnapshot(ctx, tx, &out); err != nil {
		return AccessSnapshot{}, fmt.Errorf("store: access snapshot: %w", err)
	}
	return out, nil
}

// accessRows runs one query of the snapshot and scans every row.
func accessRows(ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func accessShareRule(mode, names string) (ops.ShareRule, error) {
	r := ops.ShareRule{Mode: mode}
	if err := json.Unmarshal([]byte(names), &r.Names); err != nil {
		return r, err
	}
	if len(r.Names) == 0 {
		r.Names = nil
	}
	return r, nil
}

func readAccessSnapshot(ctx context.Context, tx *sql.Tx, out *AccessSnapshot) error {
	out.Users, out.Control, out.Shares = map[string]AccessUser{}, map[string]WorkerControlRow{}, map[string]ops.ShareDecl{}
	steps := []struct {
		query string
		scan  func(*sql.Rows) error
	}{
		{`SELECT ` + workerColumns + ` FROM workers ORDER BY name, id`, func(r *sql.Rows) error {
			w, err := scanWorker(r)
			out.Workers = append(out.Workers, w)
			return err
		}},
		{`SELECT u.id, u.username,
		   u.active = 1 AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id),
		   u.active = 1 AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'admin')
		  FROM users u`, func(r *sql.Rows) error {
			var id string
			var u AccessUser
			err := r.Scan(&id, &u.Username, &u.Active, &u.Admin)
			out.Users[id] = u
			return err
		}},
		{`SELECT worker_id, session_name, COALESCE(legacy_targets_json, ''), created_by, created_at
		  FROM master_session ORDER BY worker_id, session_name`, func(r *sql.Rows) error {
			var m MasterRow
			err := r.Scan(&m.WorkerID, &m.SessionName, &m.LegacyTargetsJSON, &m.CreatedBy, &m.CreatedAt)
			out.Masters = append(out.Masters, m)
			return err
		}},
		{`SELECT ` + boundMasterColumns + ` FROM bound_master_session ORDER BY worker_id, session_name`, func(r *sql.Rows) error {
			b, err := scanBoundMaster(r)
			out.Bound = append(out.Bound, b)
			return err
		}},
		{`SELECT target_worker_id, source_worker_id, sessions_json, created_by, created_at
		  FROM control_accept ORDER BY target_worker_id, source_worker_id`, func(r *sql.Rows) error {
			var a AcceptRow
			var sess sql.NullString
			if err := r.Scan(&a.TargetWorkerID, &a.SourceWorkerID, &sess, &a.CreatedBy, &a.CreatedAt); err != nil {
				return err
			}
			if sess.Valid {
				// As ListAccepts: an unreadable limit names no session.
				_ = json.Unmarshal([]byte(sess.String), &a.Sessions)
				if a.Sessions == nil {
					a.Sessions = []string{}
				}
			}
			out.Accepts = append(out.Accepts, a)
			return nil
		}},
		{`SELECT worker_id, user_managed, accept_all FROM worker_control`, func(r *sql.Rows) error {
			var c WorkerControlRow
			var um, all int
			err := r.Scan(&c.WorkerID, &um, &all)
			c.UserManaged, c.AcceptAll = um == 1, all == 1
			out.Control[c.WorkerID] = c
			return err
		}},
		{`SELECT worker_id, mode, names, revision FROM worker_share`, func(r *sql.Rows) error {
			var id, mode, names string
			var rev int64
			if err := r.Scan(&id, &mode, &names, &rev); err != nil {
				return err
			}
			rule, err := accessShareRule(mode, names)
			out.Shares[id] = ops.ShareDecl{Rule: rule, Revision: uint64(rev)}
			return err
		}},
		{`SELECT seq, worker_id, mode, names, base_revision FROM worker_share_pending ORDER BY seq`, func(r *sql.Rows) error {
			var p PendingShare
			var seq, base int64
			var names string
			if err := r.Scan(&seq, &p.WorkerID, &p.Rule.Mode, &names, &base); err != nil {
				return err
			}
			rule, err := accessShareRule(p.Rule.Mode, names)
			p.Seq, p.BaseRevision, p.Rule = uint64(seq), uint64(base), rule
			out.Pending = append(out.Pending, p)
			return err
		}},
		{`SELECT ` + sessionGrantCols + ` FROM session_grant ORDER BY created_at, id`, func(r *sql.Rows) error {
			g, err := scanSessionGrant(r)
			out.Grants = append(out.Grants, g)
			return err
		}},
	}
	for _, st := range steps {
		if err := accessRows(ctx, tx, st.query, st.scan); err != nil {
			return err
		}
	}
	return nil
}
