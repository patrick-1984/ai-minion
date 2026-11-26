package store

// Grants that end without a person's operation, and grant usage.
//
// A grant that ends because its session is gone or because it expired long
// ago leaves one audit row "grant.end" (actor "system"), written in the
// transaction that deletes it. The causes that SQL decides (a Worker changes
// owner or is removed, the authority behind an admin grant is lost) are
// recorded by the triggers of migration 0034 in the same form; those rows reach
// the audit observer and the log sinks through [Store.forwardSQLAudit].

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Audit vocabulary of the system causes.
const (
	// AuditActorSystem is the actor of an audit row that no person caused.
	AuditActorSystem = "system"
	// AuditActionGrantEnd records a grant that was deleted by a system cause.
	AuditActionGrantEnd = "grant.end"
	// AuditActionGrantSuspend records a grant suspended by a system cause.
	AuditActionGrantSuspend = "grant.suspend"

	GrantEndSessionGone   = "session_gone"
	GrantEndOwnerChanged  = "owner_changed"
	GrantEndWorkerRemoved = "worker_removed"
	GrantEndExpired       = "expired"
)

// grantAuditTarget is the audit target of a grant: "<target worker id>:<session
// or *>".
func grantAuditTarget(g SessionGrantRow) string {
	if g.TargetSession == "" {
		return g.TargetWorkerID + ":*"
	}
	return g.TargetWorkerID + ":" + g.TargetSession
}

// endSessionGrants deletes the given grants in tx and writes one "grant.end"
// row for each grant that was really deleted. fence, when positive, deletes
// only a grant created strictly before it (the purge fence: created_at is
// when the named sessions were bound, an edit or an approval does not move
// it). An audit insert that fails returns the error, so the caller's
// transaction rolls the delete back.
func (s *Store) endSessionGrants(ctx context.Context, tx *sql.Tx, grants []SessionGrantRow, cause string, fence int64) (int64, error) {
	var total int64
	now := time.Now().UnixMilli()
	for _, g := range grants {
		var (
			res sql.Result
			err error
		)
		if fence > 0 {
			res, err = tx.ExecContext(ctx, `DELETE FROM session_grant WHERE id = ? AND created_at < ?`, g.ID, fence)
		} else {
			res, err = tx.ExecContext(ctx, `DELETE FROM session_grant WHERE id = ?`, g.ID)
		}
		if err != nil {
			return total, fmt.Errorf("store: end session grant: %w", err)
		}
		if k, _ := res.RowsAffected(); k == 0 {
			continue
		}
		total++
		if err := s.InsertAudit(ctx, tx, AuditRow{
			ID: NewID(), TS: now, Actor: AuditActorSystem, Action: AuditActionGrantEnd,
			Target: grantAuditTarget(g), Outcome: "success",
			Source: g.SourceWorkerID + ":" + g.SourceSession, GrantID: g.ID, Detail: "cause=" + cause,
		}); err != nil {
			return total, err
		}
	}
	return total, nil
}

func queryGrantsTx(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]SessionGrantRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+sessionGrantCols+` FROM session_grant `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: select session grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionGrantRow
	for rows.Next() {
		g, err := scanSessionGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: select session grants: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// endGoneSessionGrants is [purgeSessionGrants] with an audit row per deleted
// grant (cause session_gone): same selection, same created_at fence, same
// transaction. The session purge ([Store.PurgeGoneSessions]) uses it, so a
// grant never leaves without its audit row.
func (s *Store) endGoneSessionGrants(ctx context.Context, tx *sql.Tx, workerID string, gone func(string) bool, asOf int64) (int64, error) {
	all, err := queryGrantsTx(ctx, tx, `WHERE (source_worker_id = ? OR target_worker_id = ?) AND created_at < ?`, workerID, workerID, asOf)
	if err != nil {
		return 0, err
	}
	var drop []SessionGrantRow
	for _, g := range all {
		if (g.SourceWorkerID == workerID && gone(g.SourceSession)) ||
			(g.TargetWorkerID == workerID && g.TargetSession != "" && gone(g.TargetSession)) {
			drop = append(drop, g)
		}
	}
	return s.endSessionGrants(ctx, tx, drop, GrantEndSessionGone, asOf)
}

// EndGoneSessionGrants deletes the grants that name one of the gone sessions
// of workerID as source or target and were created strictly before asOf,
// and writes one audit row "grant.end" (cause session_gone) for each, all in
// one transaction. It returns the number of grants deleted. A grant on the
// whole target Worker names no session and stays.
func (s *Store) EndGoneSessionGrants(ctx context.Context, workerID string, goneNames []string, asOf int64) (int64, error) {
	if workerID == "" || len(goneNames) == 0 {
		return 0, nil
	}
	set := make(map[string]bool, len(goneNames))
	for _, n := range goneNames {
		set[n] = true
	}
	return s.endGoneGrantsTx(ctx, workerID, func(n string) bool { return set[n] }, asOf)
}

// EndSessionGrantsNotIn is [Store.EndGoneSessionGrants] for every session of
// workerID that is not in live (the Worker's sessions after a resync).
func (s *Store) EndSessionGrantsNotIn(ctx context.Context, workerID string, live []string, asOf int64) (int64, error) {
	if workerID == "" {
		return 0, nil
	}
	set := make(map[string]bool, len(live))
	for _, n := range live {
		set[n] = true
	}
	return s.endGoneGrantsTx(ctx, workerID, func(n string) bool { return !set[n] }, asOf)
}

func (s *Store) endGoneGrantsTx(ctx context.Context, workerID string, gone func(string) bool, asOf int64) (int64, error) {
	var total int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		total, err = s.endGoneSessionGrants(ctx, tx, workerID, gone, asOf)
		return err
	})
	return total, err
}

// EndExpiredSessionGrants deletes the grants that expired before the given
// time (unix milliseconds) and writes one audit row "grant.end" (cause
// expired) for each, in one transaction. It returns their number. It is
// [Store.DeleteExpiredSessionGrants] with the audit rows.
func (s *Store) EndExpiredSessionGrants(ctx context.Context, before int64) (int64, error) {
	var total int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		grants, err := queryGrantsTx(ctx, tx, `WHERE expires_at IS NOT NULL AND expires_at < ?`, before)
		if err != nil {
			return err
		}
		total, err = s.endSessionGrants(ctx, tx, grants, GrantEndExpired, 0)
		return err
	})
	return total, err
}

// SessionGrantUse is how often and when a grant last allowed a call.
type SessionGrantUse struct {
	LastUsedAt *int64 // unix milliseconds; nil when never used
	UseCount   int64
}

// TouchSessionGrants records that the grants were used: last_used_at becomes
// at (unix milliseconds) unless a later time is stored, and use_count grows
// by n[id], or by 1 for an id that n does not name. A grant that is gone is
// skipped. Neither the revision nor updated_at changes.
func (s *Store) TouchSessionGrants(ctx context.Context, ids []string, at int64, n map[string]int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			k, ok := n[id]
			if !ok {
				k = 1
			}
			if k < 0 {
				k = 0
			}
			if _, err := tx.ExecContext(ctx, `UPDATE session_grant SET use_count = use_count + ?,
				last_used_at = CASE WHEN last_used_at IS NULL OR last_used_at < ? THEN ? ELSE last_used_at END
				WHERE id = ?`, k, at, at, id); err != nil {
				return fmt.Errorf("store: touch session grant: %w", err)
			}
		}
		return nil
	})
}

// SessionGrantUsage returns the usage of the named grants; a grant that does
// not exist is left out.
func (s *Store) SessionGrantUsage(ctx context.Context, ids ...string) (map[string]SessionGrantUse, error) {
	out := make(map[string]SessionGrantUse, len(ids))
	for _, id := range ids {
		var last sql.NullInt64
		var u SessionGrantUse
		err := s.db.QueryRowContext(ctx, `SELECT last_used_at, use_count FROM session_grant WHERE id = ?`, id).Scan(&last, &u.UseCount)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: session grant usage: %w", err)
		}
		if last.Valid {
			u.LastUsedAt = &last.Int64
		}
		out[id] = u
	}
	return out, nil
}
