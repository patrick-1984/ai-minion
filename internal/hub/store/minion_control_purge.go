package store

// Vanished sessions lose their control settings (v0.9.1, A20). A session is
// known to the Hub by its name only, and a name can come back as another
// terminal (a renamed tmux session, a new one under an old name). So when a
// session ends or stops being shared, its master nomination is deleted and it
// is taken out of every session-limited acceptance of its Worker; an
// acceptance that named only gone sessions is deleted. A name that comes back
// starts from the Worker's defaults: a slave session, accepted only where the
// Worker accepts its source as a whole.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// PurgeGoneSessions removes the control settings of the named sessions of
// workerID. It returns the number of rows deleted or changed.
func (s *Store) PurgeGoneSessions(ctx context.Context, workerID string, gone []string) (int64, error) {
	if workerID == "" || len(gone) == 0 {
		return 0, nil
	}
	set := make(map[string]bool, len(gone))
	for _, n := range gone {
		set[n] = true
	}
	return s.purgeSessions(ctx, workerID, func(n string) bool { return set[n] })
}

// PurgeSessionsNotIn removes the control settings of every session of
// workerID that is not in live (the Worker's sessions after a resync).
func (s *Store) PurgeSessionsNotIn(ctx context.Context, workerID string, live []string) (int64, error) {
	if workerID == "" {
		return 0, nil
	}
	set := make(map[string]bool, len(live))
	for _, n := range live {
		set[n] = true
	}
	return s.purgeSessions(ctx, workerID, func(n string) bool { return !set[n] })
}

func (s *Store) purgeSessions(ctx context.Context, workerID string, gone func(string) bool) (int64, error) {
	var total int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT session_name FROM master_session WHERE worker_id = ?`, workerID)
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
			res, err := tx.ExecContext(ctx, `DELETE FROM master_session WHERE worker_id = ? AND session_name = ?`, workerID, n)
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
			`SELECT source_worker_id, sessions_json FROM control_accept WHERE target_worker_id = ? AND sessions_json IS NOT NULL`, workerID)
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
			var res sql.Result
			if len(keep) == 0 {
				res, err = tx.ExecContext(ctx, `DELETE FROM control_accept WHERE target_worker_id = ? AND source_worker_id = ?`, workerID, a.source)
			} else {
				b, _ := json.Marshal(keep)
				res, err = tx.ExecContext(ctx, `UPDATE control_accept SET sessions_json = ? WHERE target_worker_id = ? AND source_worker_id = ?`, string(b), workerID, a.source)
			}
			if err != nil {
				return fmt.Errorf("store: purge acceptance: %w", err)
			}
			k, _ := res.RowsAffected()
			total += k
		}
		return nil
	})
	return total, err
}
