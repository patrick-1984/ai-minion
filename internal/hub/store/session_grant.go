package store

// Session grants (D1, D2): one row says that a source session on one Worker
// may reach a target on another Worker at a level. This file is the storage
// only; who may decide and what a grant allows is judged elsewhere.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrGrantRevision reports a write whose if_revision is not the stored one.
	ErrGrantRevision = errors.New("store: session grant revision mismatch")
	// ErrGrantOwners reports a grant whose Workers are not approved, are not
	// owned as the row says, or whose authority does not fit their owners.
	ErrGrantOwners = errors.New("store: session grant owners changed")
	// ErrGrantExists reports a second grant for the same source and target.
	ErrGrantExists = errors.New("store: session grant exists")
	// ErrGrantInvalid reports a row the table refuses (same Worker, level).
	ErrGrantInvalid = errors.New("store: session grant invalid")
)

// Grant authorities.
const (
	GrantAuthorityOwner = "owner"
	GrantAuthorityAdmin = "admin"
)

// SessionGrantRow is one row of session_grant. Times are unix milliseconds.
type SessionGrantRow struct {
	ID                string
	SourceWorkerID    string
	SourceSession     string
	SourceIncarnation string // "" = identified by name
	TargetWorkerID    string
	TargetSession     string // "" = every shared session of the target Worker
	Level             int    // 1 View, 2 Message, 3 Control
	Authority         string
	GrantedBy         string
	SourceOwnerID     string
	TargetOwnerID     string
	Note              string
	ExpiresAt         *int64
	Revision          uint64
	CreatedAt         int64
	UpdatedAt         int64
	SuspendedAt       *int64
	SuspendCause      string
}

// SessionGrantFilter narrows [Store.ListSessionGrants]. An empty member does
// not filter. WorkerID and OwnerID match the source or the target side.
type SessionGrantFilter struct {
	WorkerID string
	OwnerID  string
}

const sessionGrantCols = `id, source_worker_id, source_session, source_incarnation, target_worker_id, target_session,
	level, authority, granted_by, source_owner_id, target_owner_id, note, expires_at, revision, created_at, updated_at,
	suspended_at, suspend_cause`

func scanSessionGrant(sc rowScanner) (SessionGrantRow, error) {
	var g SessionGrantRow
	var exp, sus sql.NullInt64
	var rev int64
	err := sc.Scan(&g.ID, &g.SourceWorkerID, &g.SourceSession, &g.SourceIncarnation, &g.TargetWorkerID, &g.TargetSession,
		&g.Level, &g.Authority, &g.GrantedBy, &g.SourceOwnerID, &g.TargetOwnerID, &g.Note, &exp, &rev, &g.CreatedAt,
		&g.UpdatedAt, &sus, &g.SuspendCause)
	if err != nil {
		return g, err
	}
	g.Revision = uint64(rev)
	if exp.Valid {
		g.ExpiresAt = &exp.Int64
	}
	if sus.Valid {
		g.SuspendedAt = &sus.Int64
	}
	return g, nil
}

type sessionGrantQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func sessionGrantByID(ctx context.Context, q sessionGrantQuerier, id string) (SessionGrantRow, error) {
	g, err := scanSessionGrant(q.QueryRowContext(ctx, `SELECT `+sessionGrantCols+` FROM session_grant WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, fmt.Errorf("store: session grant: %w", err)
	}
	return g, nil
}

// pinGrantOwners refuses a grant whose Workers are missing, not approved or
// not owned as the row says, and an owner grant across two owners.
func pinGrantOwners(ctx context.Context, tx *sql.Tx, g SessionGrantRow) error {
	for _, side := range []struct{ worker, owner string }{
		{g.SourceWorkerID, g.SourceOwnerID}, {g.TargetWorkerID, g.TargetOwnerID},
	} {
		var owner, approval string
		err := tx.QueryRowContext(ctx, `SELECT owner_id, approval FROM workers WHERE id = ?`, side.worker).Scan(&owner, &approval)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGrantOwners
		}
		if err != nil {
			return fmt.Errorf("store: session grant owners: %w", err)
		}
		if owner != side.owner || approval != "approved" {
			return ErrGrantOwners
		}
	}
	if g.Authority == GrantAuthorityOwner && g.SourceOwnerID != g.TargetOwnerID {
		return ErrGrantOwners
	}
	return nil
}

// PutSessionGrant creates the grant (ifRevision 0, no row with that id) or
// changes level, expiry and note of the stored one when its revision is
// ifRevision. Both Workers' owners and approvals are checked in the same
// transaction. It returns the row as stored. An empty ID gets a new one; a
// zero UpdatedAt is now.
func (s *Store) PutSessionGrant(ctx context.Context, row SessionGrantRow, ifRevision uint64) (SessionGrantRow, error) {
	if row.ID == "" {
		row.ID = NewID()
	}
	now := row.UpdatedAt
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	var out SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := sessionGrantByID(ctx, tx, row.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			if ifRevision != 0 {
				return ErrNotFound
			}
			if row.SourceWorkerID == row.TargetWorkerID {
				return fmt.Errorf("%w: source and target on the same Worker", ErrGrantInvalid)
			}
			if err := pinGrantOwners(ctx, tx, row); err != nil {
				return err
			}
			created := row.CreatedAt
			if created == 0 {
				created = now
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO session_grant (id, source_worker_id, source_session, source_incarnation,
				target_worker_id, target_session, level, authority, granted_by, source_owner_id, target_owner_id, note,
				expires_at, revision, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
				row.ID, row.SourceWorkerID, row.SourceSession, row.SourceIncarnation, row.TargetWorkerID, row.TargetSession,
				row.Level, row.Authority, row.GrantedBy, row.SourceOwnerID, row.TargetOwnerID, row.Note,
				nullInt64(row.ExpiresAt), created, now)
			if err != nil {
				msg := err.Error()
				switch {
				case strings.Contains(msg, "UNIQUE constraint"):
					return ErrGrantExists
				case strings.Contains(msg, "CHECK constraint"):
					return fmt.Errorf("%w: %v", ErrGrantInvalid, err)
				}
				return fmt.Errorf("store: create session grant: %w", err)
			}
		case err != nil:
			return err
		default:
			if ifRevision == 0 || cur.Revision != ifRevision {
				return ErrGrantRevision
			}
			if err := pinGrantOwners(ctx, tx, cur); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE session_grant SET level = ?, expires_at = ?, note = ?,
				revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
				row.Level, nullInt64(row.ExpiresAt), row.Note, now, row.ID, int64(ifRevision)); err != nil {
				if strings.Contains(err.Error(), "CHECK constraint") {
					return fmt.Errorf("%w: %v", ErrGrantInvalid, err)
				}
				return fmt.Errorf("store: update session grant: %w", err)
			}
		}
		out, err = sessionGrantByID(ctx, tx, row.ID)
		return err
	})
	return out, err
}

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// DeleteSessionGrant deletes one grant and reports whether it existed.
func (s *Store) DeleteSessionGrant(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM session_grant WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete session grant: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ApproveSessionGrant lifts a suspension: approverUserID becomes granted_by
// and the revision goes up. The caller has checked that the approver may
// stand behind the grant.
func (s *Store) ApproveSessionGrant(ctx context.Context, id, approverUserID string, ifRevision uint64) (SessionGrantRow, error) {
	var out SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := sessionGrantByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Revision != ifRevision {
			return ErrGrantRevision
		}
		if err := pinGrantOwners(ctx, tx, cur); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE session_grant SET suspended_at = NULL, suspend_cause = '', granted_by = ?,
			revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
			approverUserID, time.Now().UnixMilli(), id, int64(ifRevision)); err != nil {
			return fmt.Errorf("store: approve session grant: %w", err)
		}
		out, err = sessionGrantByID(ctx, tx, id)
		return err
	})
	return out, err
}

// SessionGrant returns one grant or [ErrNotFound].
func (s *Store) SessionGrant(ctx context.Context, id string) (SessionGrantRow, error) {
	return sessionGrantByID(ctx, s.db, id)
}

func (s *Store) querySessionGrants(ctx context.Context, where string, args ...any) ([]SessionGrantRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionGrantCols+` FROM session_grant `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list session grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionGrantRow
	for rows.Next() {
		g, err := scanSessionGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list session grants: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListSessionGrants returns the grants that match f, oldest first.
func (s *Store) ListSessionGrants(ctx context.Context, f SessionGrantFilter) ([]SessionGrantRow, error) {
	where, args := "WHERE 1=1", []any{}
	if f.WorkerID != "" {
		where += " AND (source_worker_id = ? OR target_worker_id = ?)"
		args = append(args, f.WorkerID, f.WorkerID)
	}
	if f.OwnerID != "" {
		where += " AND (source_owner_id = ? OR target_owner_id = ?)"
		args = append(args, f.OwnerID, f.OwnerID)
	}
	return s.querySessionGrants(ctx, where, args...)
}

// SessionGrantsFor returns the grants of one source session that is
// identified by its name (source_incarnation ”). It is one read; expired and
// suspended rows are included, the caller judges them.
func (s *Store) SessionGrantsFor(ctx context.Context, sourceWorkerID, sourceSession string) ([]SessionGrantRow, error) {
	return s.querySessionGrants(ctx, `WHERE source_worker_id = ? AND source_session = ? AND source_incarnation = ''`,
		sourceWorkerID, sourceSession)
}

// PurgeSessionGrants deletes the grants that name a gone session of workerID
// as source or target and were last saved strictly before asOf. A grant on
// the whole target Worker (target_session ”) names no session and stays.
func (s *Store) PurgeSessionGrants(ctx context.Context, workerID string, goneNames []string, asOf int64) (int64, error) {
	if workerID == "" || len(goneNames) == 0 {
		return 0, nil
	}
	set := make(map[string]bool, len(goneNames))
	for _, n := range goneNames {
		set[n] = true
	}
	var total int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		total, err = purgeSessionGrants(ctx, tx, workerID, func(n string) bool { return set[n] }, asOf)
		return err
	})
	return total, err
}

func purgeSessionGrants(ctx context.Context, tx *sql.Tx, workerID string, gone func(string) bool, asOf int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, source_worker_id, source_session, target_worker_id, target_session
		FROM session_grant WHERE (source_worker_id = ? OR target_worker_id = ?) AND updated_at < ?`, workerID, workerID, asOf)
	if err != nil {
		return 0, fmt.Errorf("store: purge session grants: %w", err)
	}
	var drop []string
	for rows.Next() {
		var id, sw, ss, tw, ts string
		if err := rows.Scan(&id, &sw, &ss, &tw, &ts); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if (sw == workerID && gone(ss)) || (tw == workerID && ts != "" && gone(ts)) {
			drop = append(drop, id)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, id := range drop {
		res, err := tx.ExecContext(ctx, `DELETE FROM session_grant WHERE id = ? AND updated_at < ?`, id, asOf)
		if err != nil {
			return total, fmt.Errorf("store: purge session grant: %w", err)
		}
		k, _ := res.RowsAffected()
		total += k
	}
	return total, nil
}

// DeleteExpiredSessionGrants deletes the grants that expired before the given
// time (unix milliseconds) and returns their number.
func (s *Store) DeleteExpiredSessionGrants(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM session_grant WHERE expires_at IS NOT NULL AND expires_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired session grants: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
