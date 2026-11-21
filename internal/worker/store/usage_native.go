package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// SaveNativeUsage is called locally by an explicitly configured exporter.
// Its transaction fences registration changes and accepts only typed metadata.
func (s *Store) SaveNativeUsage(ctx context.Context, enrollment Enrollment, r ops.UsageReport, now time.Time) error {
	if err := r.Check(now); err != nil {
		return err
	}
	if len(r.Sessions) != 1 || len(r.Accounts) > 1 || len(r.Events) > 1 {
		return errors.New("native usage needs one session and at most one account and event")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var granted bool
	if err := tx.QueryRowContext(ctx, `SELECT usage_policy=1 FROM hub_enrollment WHERE id=1 AND worker_id=? AND worker_id<>'' AND owner=? AND owner<>'' AND hub_pub=?`, enrollment.WorkerID, enrollment.Owner, []byte(enrollment.HubPub)).Scan(&granted); err != nil || !granted {
		return errors.New("accept usage monitoring for this enrollment before recording metrics")
	}
	row := r.Sessions[0]
	for _, e := range r.Events {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		change, err := tx.ExecContext(ctx, `INSERT INTO usage_native_events(event_id,at,body) VALUES(?,?,?) ON CONFLICT(event_id) DO NOTHING`, e.ID, e.ObservedAt, string(b))
		if err != nil {
			return err
		}
		if n, _ := change.RowsAffected(); n == 0 && row.Source == "deepseek-response" {
			return tx.Commit()
		}
	}
	if row.Source == "deepseek-response" {
		var previous string
		err := tx.QueryRowContext(ctx, `SELECT body FROM usage_native WHERE session_name=? AND native_id=?`, row.Name, row.NativeID).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var old ops.UsageSession
			if err := json.Unmarshal([]byte(previous), &old); err != nil {
				return err
			}
			combine := func(a, b *int64) *int64 {
				if a == nil || b == nil || *a > math.MaxInt64-*b {
					return nil
				}
				v := *a + *b
				return &v
			}
			row.InputTokens = combine(row.InputTokens, old.InputTokens)
			row.OutputTokens = combine(row.OutputTokens, old.OutputTokens)
			row.CacheReadTokens = combine(row.CacheReadTokens, old.CacheReadTokens)
			row.ReasoningTokens = combine(row.ReasoningTokens, old.ReasoningTokens)
		}
	}
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	var account any
	if len(r.Accounts) > 0 {
		b, err := json.Marshal(r.Accounts[0])
		if err != nil {
			return err
		}
		account = string(b)
	}
	// A lifecycle event has its own table; it must never erase a cumulative
	// metric snapshot from the same native session.
	if row.Source != "claude-hook" {
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_native(session_name,native_id,observed_at,body,account) VALUES(?,?,?,?,?)
		 ON CONFLICT(session_name,native_id) DO UPDATE SET observed_at=excluded.observed_at,body=excluded.body,account=excluded.account WHERE excluded.observed_at>=usage_native.observed_at`, row.Name, row.NativeID, row.ObservedAt, string(body), account)
		if err != nil {
			return err
		}
	}
	cut := now.Add(-7 * 24 * time.Hour).UnixMilli()
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_native WHERE (session_name,native_id) IN (SELECT session_name,native_id FROM usage_native WHERE observed_at<? ORDER BY observed_at LIMIT 128)`, cut); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_native_events WHERE seq IN (SELECT seq FROM usage_native_events WHERE at<? ORDER BY at LIMIT 128)`, cut); err != nil {
		return err
	}
	return tx.Commit()
}

// NativeUsage returns one latest safe observation per requested host session.
// Reusing a tmux name cannot restore an earlier incarnation's native metrics.
func (s *Store) NativeUsage(ctx context.Context, report *ops.UsageReport, after int64) error {
	wanted := make(map[string]int, len(report.Sessions))
	names := make([]string, 0, len(report.Sessions))
	for i, row := range report.Sessions {
		wanted[row.Name] = i
		names = append(names, row.Name)
	}
	filter, err := json.Marshal(names)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT body,account FROM (
	SELECT body,account,row_number() OVER(PARTITION BY session_name ORDER BY observed_at DESC,native_id) rn
	FROM usage_native WHERE session_name IN (SELECT value FROM json_each(?))) WHERE rn=1`, string(filter))
	if err != nil {
		return err
	}
	known := map[string]string{}
	accountRows := map[string]ops.UsageAccount{}
	for _, a := range report.Accounts {
		accountRows[a.ID+"|"+a.SourceSessionID] = a
	}
	for rows.Next() {
		var body string
		var account sql.NullString
		if err = rows.Scan(&body, &account); err != nil {
			break
		}
		var n ops.UsageSession
		if err = json.Unmarshal([]byte(body), &n); err != nil {
			break
		}
		idx, ok := wanted[n.Name]
		if !ok {
			continue
		}
		base := report.Sessions[idx]
		if base.StartedAt == nil || n.ObservedAt < *base.StartedAt || n.ObservedAt > report.ObservedAt {
			continue
		}
		n.ID = base.ID
		n.StartedAt = base.StartedAt
		report.Sessions[idx] = n
		known[n.NativeID] = base.ID
		if account.Valid {
			var a ops.UsageAccount
			if err = json.Unmarshal([]byte(account.String), &a); err != nil {
				break
			}
			a.SourceSessionID = base.ID
			key := a.ID + "|" + a.SourceSessionID
			previous, found := accountRows[key]
			if !found || previous.Source == "native-account" || a.ObservedAt > previous.ObservedAt {
				accountRows[key] = a
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	report.Accounts = report.Accounts[:0]
	for _, a := range accountRows {
		report.Accounts = append(report.Accounts, a)
	}
	slices.SortFunc(report.Accounts, func(a, b ops.UsageAccount) int {
		if a.ID == b.ID {
			return strings.Compare(a.SourceSessionID, b.SourceSessionID)
		}
		return strings.Compare(a.ID, b.ID)
	})
	events, err := s.db.QueryContext(ctx, `SELECT seq,body FROM usage_native_events WHERE seq>? AND at<=? ORDER BY seq LIMIT 128`, after, report.ObservedAt)
	if err != nil {
		return err
	}
	defer events.Close()
	for events.Next() {
		var seq int64
		var b string
		if err := events.Scan(&seq, &b); err != nil {
			return err
		}
		var e ops.UsageEvent
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			return err
		}
		e.Seq = seq
		e.SessionID = known[e.NativeID]
		report.Events = append(report.Events, e)
	}
	return events.Err()
}
