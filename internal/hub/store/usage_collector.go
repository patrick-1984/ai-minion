package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

var ErrUsageNoNativeReader = errors.New("this account has no supported native background query")

// storedUsageAccount is an account body with the login label kept apart from
// the label of the latest observation. Readers decode it as a plain account.
type storedUsageAccount struct {
	ops.UsageAccount
	LoginLabel string `json:"login_label,omitempty"`
}

func saveUsageAccount(ctx context.Context, tx *sql.Tx, workerID string, a ops.UsageAccount, now time.Time) error {
	a.Provider = ops.UsageProvider(a.Provider)
	a.Windows = ops.SubscriptionWindows(a)
	nativeRead := a.Provider == "OpenAI" && a.AuthMode == "subscription" && (a.Source == "codex-account" || a.Source == "codex-app-server")
	if a.SourceSessionID != "" {
		a.SourceSessionID = ops.UsageID("hub-session", workerID, a.SourceSessionID)
	}
	if a.AuthMode == "subscription" && a.Source != "claude-statusline" && a.Source != "gemini-quota" && a.Source != "codex-app-server" {
		a.Source = "native-account"
		a.Windows = nil
	}
	var preferredWorker, preferredSession string
	err := tx.QueryRowContext(ctx, `SELECT coalesce(preferred_worker,''),preferred_session FROM usage_collectors WHERE account_id=?`, a.ID).Scan(&preferredWorker, &preferredSession)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if a.Source != "native-account" && (preferredWorker != "" && preferredWorker != workerID || preferredSession != "" && preferredSession != a.SourceSessionID) {
		// Retain account discovery on this Worker while the designated
		// source supplies the authoritative quota windows.
		a.Source = "native-account"
		a.Windows = nil
	}
	// The login identity comes from native discovery and is stored apart
	// from the quota observations: a quota sample of the same account carries
	// only a generic label, however many samples follow, and must not replace it.
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT body FROM usage_accounts WHERE worker_id=? AND account_id=?`, workerID, a.ID).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var old storedUsageAccount
	login := ""
	if stored != "" && json.Unmarshal([]byte(stored), &old) == nil {
		login = old.LoginLabel
		if login == "" && old.Source == "native-account" {
			login = old.Label
		}
	}
	if a.Source == "native-account" && a.Label != "" {
		login = a.Label
		if stored != "" && old.Source != "native-account" {
			if _, err := tx.ExecContext(ctx, `UPDATE usage_accounts SET body=json_set(body,'$.label',?,'$.login_label',?) WHERE worker_id=? AND account_id=?`, a.Label, a.Label, workerID, a.ID); err != nil {
				return err
			}
		}
	} else if a.Source != "native-account" && login != "" {
		a.Label = login
	}
	// Status-line limits merge per window with the stored reading of this
	// Worker (the later reset wins, then the higher percentage) instead of
	// the latest write replacing it: sessions re-send old readings.
	merged := false
	if stored != "" && ops.MergeableUsageAccounts(old.UsageAccount, a) {
		a.Windows = ops.MergeUsageWindows(now, a.Windows, old.Windows)
		a.ObservedAt = max(a.ObservedAt, old.ObservedAt)
		merged = true
	}
	b, err := json.Marshal(storedUsageAccount{UsageAccount: a, LoginLabel: login})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_accounts(worker_id,account_id,observed_at,body,can_poll) VALUES(?,?,?,?,?)
	 ON CONFLICT(worker_id,account_id) DO UPDATE SET observed_at=excluded.observed_at,body=excluded.body,can_poll=max(usage_accounts.can_poll,excluded.can_poll)
	 WHERE (excluded.observed_at>usage_accounts.observed_at OR ?) AND
	  (json_extract(excluded.body,'$.source')<>'native-account' OR json_extract(usage_accounts.body,'$.source')='native-account')`, workerID, a.ID, a.ObservedAt, string(b), nativeRead, merged)
	if err == nil && nativeRead {
		_, err = tx.ExecContext(ctx, `UPDATE usage_accounts SET can_poll=1 WHERE worker_id=? AND account_id=? AND can_poll=0`, workerID, a.ID)
	}
	if err != nil || a.AuthMode != "subscription" {
		return err
	}
	status := "passive"
	if nativeRead {
		status = "waiting"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_collectors(account_id,status) VALUES(?,?) ON CONFLICT DO NOTHING`, a.ID, status)
	if err == nil && a.SourceSessionID != "" && a.Source != "native-account" {
		_, err = tx.ExecContext(ctx, `UPDATE usage_collectors SET active_worker=?,last_success=?,hide_previous_schedule=0,status='passive' WHERE account_id=? AND lease_id='' AND (last_success IS NULL OR last_success<?)`, workerID, a.ObservedAt, a.ID, a.ObservedAt)
	}
	return err
}

type UsageLease struct{ ID, AccountID, WorkerID, OwnerID string }

// ClaimUsagePoll puts a short cross-Worker lease on a due native query. Two
// Workers seeing the same login cannot both claim it. A crash consumes the
// five-minute interval even if the provider received a request before failure.
func (s *Store) ClaimUsagePoll(ctx context.Context, workerID, owner string, now time.Time) (lease UsageLease, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var account string
		err := tx.QueryRowContext(ctx, `SELECT c.account_id FROM usage_collectors c
		 JOIN usage_accounts a ON a.account_id=c.account_id AND a.worker_id=? JOIN workers w ON w.id=a.worker_id
		 WHERE w.owner_id=? AND w.approval='approved' AND a.can_poll=1 AND c.preferred_session=''
		   AND (c.preferred_worker IS NULL OR c.preferred_worker=w.id) AND c.next_due<=? AND c.lease_until<=?
		   AND json_extract(a.body,'$.provider')='OpenAI' AND json_extract(a.body,'$.auth_mode')='subscription'
		 ORDER BY c.requested DESC,c.next_due,c.account_id LIMIT 1`, workerID, owner, now.UnixMilli(), now.UnixMilli()).Scan(&account)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		lease = UsageLease{ID: NewID(), AccountID: account, WorkerID: workerID, OwnerID: owner}
		_, err = tx.ExecContext(ctx, `UPDATE usage_collectors SET active_worker=?,lease_id=?,lease_until=?,next_due=?,last_probe=?,requested=0,hide_previous_schedule=0,status='querying' WHERE account_id=?`, workerID, lease.ID, now.Add(time.Minute).UnixMilli(), now.Add(5*time.Minute).UnixMilli(), now.UnixMilli(), account)
		return err
	})
	return lease, err
}

// CompleteUsagePoll never revives a replaced lease or writes a report from
// an enrollment that changed owner. Retry-After is a lower bound.
func (s *Store) CompleteUsagePoll(ctx context.Context, lease UsageLease, a *ops.UsageAccount, status string, retryAfter time.Duration, now time.Time) error {
	if a != nil {
		r := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: now.UnixMilli(), Accounts: []ops.UsageAccount{*a}}
		if err := r.Check(now); err != nil {
			return err
		}
		if a.ID != lease.AccountID {
			return ErrNotFound
		}
	}
	switch status {
	case "ready", "unavailable", "rate_limited", "account_changed", "unsupported":
	default:
		return errors.New("invalid collector status")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var failures int
		err := tx.QueryRowContext(ctx, `SELECT failures FROM usage_collectors c JOIN workers w ON w.id=c.active_worker WHERE c.account_id=? AND lease_id=? AND w.id=? AND w.owner_id=? AND w.approval='approved'`, lease.AccountID, lease.ID, lease.WorkerID, lease.OwnerID).Scan(&failures)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == "account_changed" {
			if _, err := tx.ExecContext(ctx, `UPDATE usage_accounts SET can_poll=0 WHERE worker_id=? AND account_id=?`, lease.WorkerID, lease.AccountID); err != nil {
				return err
			}
		}
		var success any
		delay := 5 * time.Minute
		if a != nil {
			if err = saveUsageAccount(ctx, tx, lease.WorkerID, *a, now); err != nil {
				return err
			}
			success = a.ObservedAt
			failures = 0
		} else {
			failures = min(8, failures+1)
			delay = min(6*time.Hour, delay*time.Duration(1<<failures))
		}
		delay = max(retryAfter, delay) + time.Duration(rand.IntN(15000))*time.Millisecond
		_, err = tx.ExecContext(ctx, `UPDATE usage_collectors SET lease_id='',lease_until=0,next_due=?,status=?,failures=?,last_success=coalesce(?,last_success) WHERE account_id=?`, now.Add(delay).UnixMilli(), status, failures, success, lease.AccountID)
		return err
	})
}

func (s *Store) SetUsageCollector(ctx context.Context, workers []ops.WorkerInfo, a ops.UsageCollectorSetArgs) (ops.UsageCollector, error) {
	if err := a.Validate(); err != nil {
		return ops.UsageCollector{}, err
	}
	visible, err := usageVisibleJSON(workers)
	if err != nil {
		return ops.UsageCollector{}, err
	}
	var result ops.UsageCollector
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if a.KeepSource {
			// Read-modify-write: the saved source, not the one a page showed.
			var w string
			err := tx.QueryRowContext(ctx, `SELECT coalesce(preferred_worker,''),preferred_session FROM usage_collectors WHERE account_id=?`, a.AccountID).Scan(&w, &a.SessionID)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			a.WorkerID = w
			if a.SessionID != "" {
				a.Start = false
			}
		}
		result = ops.UsageCollector{PreferredWorker: a.WorkerID, PreferredSession: a.SessionID}
		// Every source of a shared credential must be in this caller's
		// authorized Worker set to alter its domain-wide polling schedule.
		var allowed bool
		err := tx.QueryRowContext(ctx, usageVisible+`SELECT EXISTS(SELECT 1 FROM usage_accounts a JOIN visible v ON v.id=a.worker_id WHERE a.account_id=? AND (?='' OR a.worker_id=?))
		  AND NOT EXISTS(SELECT 1 FROM usage_accounts a WHERE a.account_id=? AND a.worker_id NOT IN (SELECT id FROM visible))
		  AND (?='' OR EXISTS(SELECT 1 FROM usage_sessions s WHERE s.worker_id=? AND s.id=? AND s.ended_at IS NULL AND json_extract(s.body,'$.account_id')=?))`, visible, "", "", a.AccountID, a.WorkerID, a.WorkerID, a.AccountID, a.SessionID, a.WorkerID, a.SessionID, a.AccountID).Scan(&allowed)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNotFound
		}
		var capable bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_accounts WHERE account_id=? AND can_poll=1 AND (?='' OR worker_id=?))`, a.AccountID, a.WorkerID, a.WorkerID).Scan(&capable); err != nil {
			return err
		}
		if a.KeepSource && !capable {
			a.Start = false
		}
		if a.Start && !capable {
			return ErrUsageNoNativeReader
		}
		r, err := tx.ExecContext(ctx, `WITH target(w,s,start,native,now) AS (VALUES(?,?,?,?,?))
		 UPDATE usage_collectors SET
		 preferred_worker=nullif(target.w,''),preferred_session=target.s,
		 lease_id=CASE WHEN coalesce(preferred_worker,'')=target.w AND preferred_session=target.s THEN lease_id ELSE '' END,
		 lease_until=CASE WHEN coalesce(preferred_worker,'')=target.w AND preferred_session=target.s THEN lease_until ELSE 0 END,
		 requested=max(requested,target.start),
		 next_due=CASE WHEN target.start AND failures=0 AND lease_id='' THEN min(next_due,last_probe+300000) ELSE next_due END,
		 status=CASE WHEN target.s<>'' OR NOT target.native THEN 'passive'
		   WHEN coalesce(preferred_worker,'')=target.w AND preferred_session=target.s AND (lease_id<>'' OR (failures>0 AND next_due>target.now)) THEN status
		   ELSE 'waiting' END
		 FROM target WHERE account_id=?`, a.WorkerID, a.SessionID, a.Start, capable, time.Now().UnixMilli(), a.AccountID)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrNotFound
		}
		result.CanStart = capable
		result.Mode = "passive_events"
		if capable && a.SessionID == "" {
			result.Mode = "native_background"
		}
		err = tx.QueryRowContext(ctx, `SELECT coalesce(active_worker,''),status,CASE WHEN hide_previous_schedule=0 THEN nullif(next_due,0) END,last_success FROM usage_collectors WHERE account_id=?`, a.AccountID).Scan(&result.ActiveWorker, &result.Status, &result.NextDue, &result.LastSuccess)
		return err
	})
	return result, err
}
