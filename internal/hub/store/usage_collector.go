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

func saveUsageAccount(ctx context.Context, tx *sql.Tx, workerID string, a ops.UsageAccount) error {
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
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_accounts(worker_id,account_id,observed_at,body) VALUES(?,?,?,?)
	 ON CONFLICT(worker_id,account_id) DO UPDATE SET observed_at=excluded.observed_at,body=excluded.body
	 WHERE excluded.observed_at>usage_accounts.observed_at AND
	  (json_extract(excluded.body,'$.source')<>'native-account' OR json_extract(usage_accounts.body,'$.source')='native-account')`, workerID, a.ID, a.ObservedAt, string(b))
	if err != nil || a.AuthMode != "subscription" {
		return err
	}
	status := "passive"
	if a.Provider == "OpenAI" {
		status = "waiting"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_collectors(account_id,status) VALUES(?,?) ON CONFLICT DO NOTHING`, a.ID, status)
	if err == nil && a.SourceSessionID != "" && a.Source != "native-account" {
		_, err = tx.ExecContext(ctx, `UPDATE usage_collectors SET active_worker=?,last_success=?,status='passive' WHERE account_id=? AND lease_id='' AND (last_success IS NULL OR last_success<?)`, workerID, a.ObservedAt, a.ID, a.ObservedAt)
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
		 WHERE w.owner_id=? AND w.approval='approved' AND c.status<>'passive' AND c.preferred_session=''
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
		_, err = tx.ExecContext(ctx, `UPDATE usage_collectors SET active_worker=?,lease_id=?,lease_until=?,next_due=?,last_probe=?,requested=0,status='querying' WHERE account_id=?`, workerID, lease.ID, now.Add(time.Minute).UnixMilli(), now.Add(5*time.Minute).UnixMilli(), now.UnixMilli(), account)
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
		var success any
		delay := 5 * time.Minute
		if a != nil {
			if err = saveUsageAccount(ctx, tx, lease.WorkerID, *a); err != nil {
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
	result := ops.UsageCollector{PreferredWorker: a.WorkerID, PreferredSession: a.SessionID}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
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
		r, err := tx.ExecContext(ctx, `UPDATE usage_collectors SET preferred_worker=nullif(?,''),preferred_session=?,lease_id='',lease_until=0,
		  status=CASE WHEN ?<>'' OR NOT EXISTS (SELECT 1 FROM usage_accounts a WHERE a.account_id=usage_collectors.account_id AND json_extract(a.body,'$.provider')='OpenAI') THEN 'passive' ELSE 'waiting' END
		  WHERE account_id=?`, a.WorkerID, a.SessionID, a.SessionID, a.AccountID)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err == nil && n != 1 {
			return ErrNotFound
		}
		return err
	})
	return result, err
}
