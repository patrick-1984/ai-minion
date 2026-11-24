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
	defer func() { _ = tx.Rollback() }()
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
	if len(r.Accounts) == 0 && row.Source != "claude-hook" && (row.AuthMode == "unknown" || row.AuthMode == "") {
		// A session that was seen on an API key keeps that account when a later
		// document of it carries no access evidence.
		if err := keepAPIAccount(ctx, tx, &row, &r); err != nil {
			return err
		}
	}
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	var account any
	if len(r.Accounts) > 0 {
		if row.Source != "claude-hook" {
			if err := keepClaudeWindows(ctx, tx, &r.Accounts[0], now); err != nil {
				return err
			}
		}
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
	sessionAccount := map[string]string{} // the account each native session finally has
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
		if n.Folder == "" {
			n.Folder = base.Folder
		}
		if n.Harness == "" {
			n.Harness = base.Harness
		}
		if n.Version == "" {
			n.Version = base.Version
		}
		// The access read from the session's own process wins over a profile
		// default: a session that uses an API key or a custom endpoint is not
		// the profile's subscription, whatever the status line (which knows
		// only the profile's login) says. The subscription account of that
		// observation is dropped with it.
		conflict := base.AuthMode == "api" && n.AuthMode == "subscription"
		if conflict {
			n.AuthMode, n.AccountID, n.Provider, n.CustomEndpoint = base.AuthMode, base.AccountID, base.Provider, base.CustomEndpoint
		}
		if n.AuthMode == "unknown" || n.AuthMode == "" {
			// The native document names no credential; keep the access kind
			// the Worker read from the session's own process.
			n.AuthMode, n.CustomEndpoint = base.AuthMode, base.CustomEndpoint
			if n.AccountID == "" {
				n.AccountID = base.AccountID
			}
			if n.Provider == "" {
				n.Provider = base.Provider
			}
		}
		if n.AuthMode == "api" && n.AccountID == "" && base.AuthMode == "api" {
			// Older exporters recorded API access without the endpoint kind.
			n.AccountID, n.CustomEndpoint = base.AccountID, base.CustomEndpoint
		}
		if n.AuthMode == "api" && n.AccountID == "" {
			// Rows saved by v0.20.0 name no account: group them in the
			// aggregate API account of their provider, so the totals stay.
			provider := n.Provider
			if provider == "" {
				provider = base.Provider
			}
			if provider != "" {
				legacy := AggregateAPIAccount(provider, n.CustomEndpoint, n.ObservedAt)
				legacy.SourceSessionID = base.ID
				n.AccountID, n.Provider = legacy.ID, provider
				if _, ok := accountRows[legacy.ID+"|"+base.ID]; !ok && !account.Valid {
					accountRows[legacy.ID+"|"+base.ID] = legacy
				}
			}
		}
		n.ID = base.ID
		n.StartedAt = base.StartedAt
		report.Sessions[idx] = n
		known[n.NativeID] = base.ID
		sessionAccount[n.NativeID] = n.AccountID
		if account.Valid && !conflict {
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
	_ = rows.Close()
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
	defer func() { _ = events.Close() }()
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
		// An event never names an account other than its session's own: a
		// session corrected to API access must not feed a subscription bucket.
		if acct, ok := sessionAccount[e.NativeID]; ok && e.AccountID != acct {
			e.AccountID = acct
		}
		report.Events = append(report.Events, e)
	}
	return events.Err()
}

// keepAPIAccount gives a document without access evidence the API account its
// native session already had.
func keepAPIAccount(ctx context.Context, tx *sql.Tx, row *ops.UsageSession, r *ops.UsageReport) error {
	var body string
	var account sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT body,account FROM usage_native WHERE session_name=? AND native_id=?`, row.Name, row.NativeID).Scan(&body, &account)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var old ops.UsageSession
	if json.Unmarshal([]byte(body), &old) != nil || old.AuthMode != "api" || old.AccountID == "" || !account.Valid {
		return nil
	}
	var a ops.UsageAccount
	if json.Unmarshal([]byte(account.String), &a) != nil || a.ID != old.AccountID {
		return nil
	}
	row.AuthMode, row.AccountID, row.Provider, row.CustomEndpoint = old.AuthMode, old.AccountID, old.Provider, old.CustomEndpoint
	a.ObservedAt, a.TimeSource = row.ObservedAt, row.TimeSource
	r.Accounts = []ops.UsageAccount{a}
	return nil
}

// keepClaudeWindows carries the account windows that the newest status lines
// of the same verified account (any native session, same plan) reported over a
// report that no longer has them: Claude Code drops a window until it reads a
// new one, and a capped account must not look uncapped. A carried window keeps
// its last value and its reset time. Once that time has passed it reads as
// reset (0% since the reset, reset time unchanged): it is never a fresh
// reading stamped with the time of this observation, and a new report that
// carries the window replaces it.
func keepClaudeWindows(ctx context.Context, tx *sql.Tx, a *ops.UsageAccount, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT account FROM usage_native WHERE json_extract(account,'$.id')=? ORDER BY observed_at DESC LIMIT 16`, a.ID)
	if err != nil {
		return err
	}
	var previous []string
	for rows.Next() {
		var body sql.NullString
		if err := rows.Scan(&body); err != nil {
			_ = rows.Close()
			return err
		}
		if body.Valid {
			previous = append(previous, body.String)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	type key struct {
		scope, period string
		duration      int64
	}
	keyOf := func(w ops.UsageWindow) key {
		k := key{scope: w.Scope, period: w.Period}
		if w.DurationMS != nil {
			k.duration = *w.DurationMS
		}
		return k
	}
	have := map[key]bool{}
	for _, w := range a.Windows {
		have[keyOf(w)] = true
	}
	for _, text := range previous {
		var old ops.UsageAccount
		if json.Unmarshal([]byte(text), &old) != nil || old.ID != a.ID || old.Source != "claude-statusline" || old.Plan != a.Plan {
			continue // another plan: its limits no longer apply
		}
		for _, w := range old.Windows {
			if have[keyOf(w)] || len(a.Windows) >= 32 {
				continue
			}
			have[keyOf(w)] = true
			if w.ResetsAt != nil && *w.ResetsAt <= now.UnixMilli() {
				zero := 0.0
				w.UsedPercent, w.Used, w.Remaining = &zero, nil, nil
			}
			a.Windows = append(a.Windows, w)
		}
	}
	if len(a.Windows) > 0 {
		a.Source = "claude-statusline"
	}
	return nil
}
