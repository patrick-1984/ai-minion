package store

// Vanished sessions lose their control settings (v0.9.1, A20). A session is
// known to the Hub by its name only, and a name can come back as another
// terminal (a renamed tmux session, a new one under an old name). So when a
// session ends or stops being shared, its master nomination is deleted and it
// is taken out of every session-limited acceptance of its Worker and of every
// migrated (legacy) target limit. A list that loses its last session stays as
// an empty list, which allows nothing: it is never widened to the Worker or to
// "accepts all Workers". A name that comes back is a slave session.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Every purge takes the time of the evidence (asOf, unix milliseconds: when
// the Hub saw the session end or applied the snapshot) and changes only rows
// saved at or before it: a nomination or an acceptance saved later was made
// for the session as it is now and stays.

// PurgeGoneSessions removes the control settings of the named sessions of
// workerID. It returns the number of rows deleted or changed.
func (s *Store) PurgeGoneSessions(ctx context.Context, workerID string, gone []string, asOf int64) (int64, error) {
	if workerID == "" || len(gone) == 0 {
		return 0, nil
	}
	set := make(map[string]bool, len(gone))
	for _, n := range gone {
		set[n] = true
	}
	return s.purgeSessions(ctx, workerID, func(n string) bool { return set[n] }, asOf)
}

// PurgeSessionsNotIn removes the control settings of every session of
// workerID that is not in live (the Worker's sessions after a resync).
func (s *Store) PurgeSessionsNotIn(ctx context.Context, workerID string, live []string, asOf int64) (int64, error) {
	if workerID == "" {
		return 0, nil
	}
	set := make(map[string]bool, len(live))
	for _, n := range live {
		set[n] = true
	}
	return s.purgeSessions(ctx, workerID, func(n string) bool { return !set[n] }, asOf)
}

func (s *Store) purgeSessions(ctx context.Context, workerID string, gone func(string) bool, asOf int64) (int64, error) {
	var total int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT session_name FROM master_session WHERE worker_id = ? AND created_at <= ?`, workerID, asOf)
		if err != nil {
			return fmt.Errorf("store: purge sessions: %w", err)
		}
		var drop []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()
				return err
			}
			if gone(n) {
				drop = append(drop, n)
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, n := range drop {
			res, err := tx.ExecContext(ctx, `DELETE FROM master_session WHERE worker_id = ? AND session_name = ? AND created_at <= ?`, workerID, n, asOf)
			if err != nil {
				return fmt.Errorf("store: purge master session: %w", err)
			}
			k, _ := res.RowsAffected()
			total += k
		}
		type acc struct {
			source string
			names  []string
		}
		var limited []acc
		arows, err := tx.QueryContext(ctx,
			`SELECT source_worker_id, sessions_json FROM control_accept WHERE target_worker_id = ? AND sessions_json IS NOT NULL AND created_at <= ?`, workerID, asOf)
		if err != nil {
			return fmt.Errorf("store: purge acceptances: %w", err)
		}
		for arows.Next() {
			var a acc
			var js string
			if err := arows.Scan(&a.source, &js); err != nil {
				_ = arows.Close()
				return err
			}
			_ = json.Unmarshal([]byte(js), &a.names)
			limited = append(limited, a)
		}
		_ = arows.Close()
		if err := arows.Err(); err != nil {
			return err
		}
		for _, a := range limited {
			var keep []string
			for _, n := range a.names {
				if !gone(n) {
					keep = append(keep, n)
				}
			}
			if len(keep) == len(a.names) {
				continue
			}
			if keep == nil {
				keep = []string{} // allows nothing; never NULL (= the whole Worker)
			}
			b, _ := json.Marshal(keep)
			res, err := tx.ExecContext(ctx, `UPDATE control_accept SET sessions_json = ? WHERE target_worker_id = ? AND source_worker_id = ? AND created_at <= ?`, string(b), workerID, a.source, asOf)
			if err != nil {
				return fmt.Errorf("store: purge acceptance: %w", err)
			}
			k, _ := res.RowsAffected()
			total += k
		}
		n, err := purgeLegacyTargets(ctx, tx, workerID, gone, asOf)
		total += n
		return err
	})
	return total, err
}

// purgeLegacyTargets takes the gone sessions of workerID out of every
// migrated target limit. A limit that loses its last target becomes "[]",
// which allows nothing.
func purgeLegacyTargets(ctx context.Context, tx *sql.Tx, workerID string, gone func(string) bool, asOf int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT worker_id, session_name, legacy_targets_json FROM master_session WHERE legacy_targets_json IS NOT NULL AND created_at <= ?`, asOf)
	if err != nil {
		return 0, fmt.Errorf("store: purge legacy targets: %w", err)
	}
	type upd struct{ w, s, js string }
	var todo []upd
	for rows.Next() {
		var w, sn, js string
		if err := rows.Scan(&w, &sn, &js); err != nil {
			_ = rows.Close()
			return 0, err
		}
		var lim []controlResource
		if json.Unmarshal([]byte(js), &lim) != nil {
			continue // unreadable: it already allows nothing
		}
		keep := make([]controlResource, 0, len(lim))
		for _, r := range lim {
			if r.Kind == "session" && r.WorkerID == workerID && gone(r.SessionName) {
				continue
			}
			keep = append(keep, r)
		}
		if len(keep) != len(lim) {
			b, _ := json.Marshal(keep)
			todo = append(todo, upd{w, sn, string(b)})
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, u := range todo {
		res, err := tx.ExecContext(ctx, `UPDATE master_session SET legacy_targets_json = ? WHERE worker_id = ? AND session_name = ? AND created_at <= ?`, u.js, u.w, u.s, asOf)
		if err != nil {
			return total, fmt.Errorf("store: purge legacy target: %w", err)
		}
		k, _ := res.RowsAffected()
		total += k
	}
	return total, nil
}
