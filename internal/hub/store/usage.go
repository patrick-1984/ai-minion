package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// SaveUsage binds the report to an approved Worker's authenticated identity.
// expectedOwner fences a report received across reassignment. A replayed or
// out-of-order report must never close a session that a newer report observed.
func (s *Store) SaveUsage(ctx context.Context, workerID, expectedOwner string, report ops.UsageReport, now time.Time) error {
	if err := report.Check(now); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var owner string
		var seen int64
		err := tx.QueryRowContext(ctx, `SELECT w.owner_id,coalesce(u.observed_at,0) FROM workers w LEFT JOIN usage_workers u ON w.id=u.worker_id WHERE w.id=? AND w.approval='approved'`, workerID).Scan(&owner, &seen)
		if errors.Is(err, sql.ErrNoRows) || err == nil && owner != expectedOwner {
			return ErrWorkerNotFound
		}
		if err != nil || report.ObservedAt <= seen {
			return err
		}
		var inventory any
		if report.Inventory != nil {
			b, err := json.Marshal(report.Inventory)
			if err != nil {
				return err
			}
			inventory = string(b)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_workers(worker_id,observed_at,inventory,inventory_at,complete_at) VALUES(?,?,?,?,CASE WHEN ? THEN ? ELSE 0 END)
			ON CONFLICT(worker_id) DO UPDATE SET inventory_at=coalesce(excluded.inventory_at,usage_workers.inventory_at),status='ready',observed_at=excluded.observed_at,complete_at=max(usage_workers.complete_at,excluded.complete_at),inventory=coalesce(excluded.inventory,usage_workers.inventory)`, workerID, report.ObservedAt, inventory, report.InventoryAt, report.Complete, report.ObservedAt)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(report.Sessions))
		for _, row := range report.Sessions {
			row.ID = ops.UsageID("hub-session", workerID, row.ID)
			ids = append(ids, row.ID)
			if err := saveUsageSession(ctx, tx, workerID, row); err != nil {
				return err
			}
		}
		if report.Complete {
			live, err := json.Marshal(ids)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE usage_sessions SET ended_at=? WHERE worker_id=? AND ended_at IS NULL AND id NOT IN (SELECT value FROM json_each(?))`, report.ObservedAt, workerID, string(live))
			if err != nil {
				return err
			}
		}
		for _, event := range report.Events {
			if event.Seq < 1 {
				return errors.New("usage event missing sequence")
			}
			if event.SessionID != "" {
				event.SessionID = ops.UsageID("hub-session", workerID, event.SessionID)
			}
			if event.NativeID != "" {
				event.NativeID = ops.UsageID("hub-native-session", workerID, event.NativeID)
			}
			body, err := json.Marshal(event)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO usage_events(worker_id,seq,session_id,observed_at,body) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, workerID, event.Seq, event.SessionID, event.ObservedAt, string(body))
			if err != nil {
				return err
			}
			if event.AccountID != "" && (event.Kind == "task_finished" || event.Kind == "subagent_finished" || event.Kind == "session_stopped") {
				_, err = tx.ExecContext(ctx, `UPDATE usage_collectors SET requested=1,
				 next_due=CASE WHEN failures=0 AND lease_id='' THEN min(next_due,last_probe+300000) ELSE next_due END
				 WHERE account_id=? AND EXISTS(SELECT 1 FROM usage_accounts WHERE account_id=? AND worker_id=?)`, event.AccountID, event.AccountID, workerID)
				if err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `UPDATE usage_workers SET event_cursor=max(event_cursor,?) WHERE worker_id=?`, event.Seq, workerID)
			if err != nil {
				return err
			}
		}
		for _, h := range report.Inventory {
			if h.Name == "codex" && (h.AccountStatus == "configured" || h.AccountStatus == "signed_out") {
				if _, err := tx.ExecContext(ctx, `UPDATE usage_accounts SET can_poll=0 WHERE worker_id=? AND can_poll=1`, workerID); err != nil {
					return err
				}
			}
		}
		for _, a := range report.Accounts {
			if err := saveUsageAccount(ctx, tx, workerID, a); err != nil {
				return err
			}
		}
		return nil
	})
}

func saveUsageSession(ctx context.Context, tx *sql.Tx, workerID string, row ops.UsageSession) error {
	row.Provider = ops.UsageProvider(row.Provider)
	at := row.ObservedAt
	row.ObservedAt = 0 // stored separately; skip a rewrite when only the sample time changed
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_sessions(id,worker_id,observed_at,ended_at,body) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET observed_at=excluded.observed_at,ended_at=excluded.ended_at,body=excluded.body
		WHERE excluded.observed_at>=coalesce(usage_sessions.ended_at,usage_sessions.observed_at) AND (excluded.body<>usage_sessions.body OR usage_sessions.ended_at IS NOT excluded.ended_at)`, row.ID, workerID, at, row.StoppedAt, string(body))
	if err != nil {
		return err
	}
	if row.UsageCounters == (ops.UsageCounters{}) {
		return nil
	}
	counters, err := json.Marshal(row.UsageCounters)
	if err != nil {
		return err
	}
	const hourMS = int64(time.Hour / time.Millisecond)
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_hours(session_id,hour,observed_at,counters,account_id) VALUES(?,?,?,?,?)
		ON CONFLICT(session_id,hour) DO UPDATE SET observed_at=excluded.observed_at,counters=excluded.counters,account_id=excluded.account_id
		WHERE excluded.observed_at>=usage_hours.observed_at AND (excluded.counters<>usage_hours.counters OR excluded.account_id<>usage_hours.account_id)`, row.ID, at/hourMS, at, string(counters), row.AccountID)
	return err
}

// TrimUsage keeps closed-session detail for 90 days and hourly snapshots for
// 30 days. Each statement limits the rows selected, allowing ingestion to
// resume between periodic maintenance runs.
func (s *Store) TrimUsage(ctx context.Context, now time.Time) error {
	const batch = 256
	// A busy 50-session installation can finish 600 events per minute.
	// Give expiration enough throughput while releasing the writer lock
	// after each batch. Short backlogs finish in one maintenance tick.
	for range 16 {
		result, err := s.db.ExecContext(ctx, `DELETE FROM usage_events WHERE id IN (SELECT id FROM usage_events WHERE observed_at<? ORDER BY observed_at LIMIT ?)`, now.Add(-7*24*time.Hour).UnixMilli(), batch)
		if err != nil {
			return err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if removed < batch {
			break
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM usage_hours WHERE (session_id,hour) IN (SELECT session_id,hour FROM usage_hours WHERE hour<? ORDER BY hour LIMIT ?)`, now.Add(-30*24*time.Hour).UnixMilli()/3600000, batch)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM usage_sessions WHERE id IN (SELECT id FROM usage_sessions WHERE ended_at<? ORDER BY ended_at LIMIT ?)`, now.Add(-90*24*time.Hour).UnixMilli(), batch)
	return err
}

func (s *Store) UsageCursor(ctx context.Context, id string) (int64, error) {
	var cursor int64
	err := s.db.QueryRowContext(ctx, `SELECT event_cursor FROM usage_workers WHERE worker_id=?`, id).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return cursor, err
}

func (s *Store) UsageFailure(ctx context.Context, workerID, owner, status string) error {
	switch status {
	case "registration_required", "unavailable":
	default:
		return errors.New("invalid Worker usage status")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_workers(worker_id,observed_at,status)
	 SELECT id,0,? FROM workers WHERE id=? AND owner_id=? AND approval='approved'
	 ON CONFLICT(worker_id) DO UPDATE SET status=excluded.status WHERE status<>excluded.status`, status, workerID, owner)
	return err
}
