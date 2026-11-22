package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// UsageBuckets always uses the authenticated caller's own ID, including for
// an administrator. This table has no domain-wide browsing operation.
func (s *Store) UsageBuckets(ctx context.Context, owner string) ([]ops.UsageBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,label,position FROM usage_buckets WHERE owner_id=? ORDER BY kind,position,label,id`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ops.UsageBucket{}
	for rows.Next() {
		var b ops.UsageBucket
		if err := rows.Scan(&b.ID, &b.Kind, &b.Label, &b.Position); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Your own published slots only; the domain summary is a separate op.
	noms, err := s.db.QueryContext(ctx, `SELECT bucket_id,scope FROM usage_nominations WHERE owner_id=? ORDER BY scope`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = noms.Close() }()
	flags := map[string][]string{}
	for noms.Next() {
		var id, scope string
		if err := noms.Scan(&id, &scope); err != nil {
			return nil, err
		}
		flags[id] = append(flags[id], scope)
	}
	for i := range out {
		out[i].Nominated = flags[out[i].ID]
	}
	return out, noms.Err()
}

func (s *Store) SetUsageBucket(ctx context.Context, owner string, authorized []ops.WorkerInfo, args ops.UsageBucketSetArgs) (ops.UsageBucketSetResult, error) {
	return s.SetUsageBucketAs(ctx, owner, authorized, args, BucketAuthority{})
}

// SetUsageBucketAs is [Store.SetUsageBucket] for a caller who may hold the
// authority to change a bucket that is currently published to the whole
// domain. Renaming, deleting or moving members into or out of such a bucket
// changes what every Worker shows, so it needs the same authority as the
// nomination itself. A bucket that is nominated may also never end up holding
// sources whose owners refused republishing.
func (s *Store) SetUsageBucketAs(ctx context.Context, owner string, authorized []ops.WorkerInfo, args ops.UsageBucketSetArgs, auth BucketAuthority) (ops.UsageBucketSetResult, error) {
	result := ops.UsageBucketSetResult{ID: args.BucketID}
	if err := args.Validate(); err != nil {
		return result, err
	}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if args.BucketID != "" {
			var exists int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM usage_buckets WHERE owner_id=? AND kind=? AND id=?`, owner, args.Kind, args.BucketID).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		locked, err := domainBucketAffected(ctx, tx, owner, authorized, args)
		if err != nil {
			return err
		}
		if locked {
			if !auth.Domain || auth.Actor == "" {
				return ErrNominationForbidden
			}
			ok, err := activeAdminTx(ctx, tx, auth.Actor)
			if err != nil {
				return err
			}
			if !ok {
				return ErrNominationForbidden
			}
			if err := s.usageAudit(ctx, tx, auth.Actor, "usage.domain_bucket."+args.Action, "domain/"+args.Kind+"/"+args.BucketID); err != nil {
				return err
			}
		}
		var change sql.Result
		switch args.Action {
		case "save":
			if args.BucketID == "" {
				result.ID = NewID()
				change, err = tx.ExecContext(ctx, `INSERT INTO usage_buckets(id,owner_id,kind,label,position) VALUES(?,?,?,?,?)`, result.ID, owner, args.Kind, strings.TrimSpace(args.Label), args.Position)
			} else {
				change, err = tx.ExecContext(ctx, `UPDATE usage_buckets SET label=?,position=? WHERE owner_id=? AND kind=? AND id=?`, strings.TrimSpace(args.Label), args.Position, owner, args.Kind, args.BucketID)
			}
		case "delete":
			change, err = tx.ExecContext(ctx, `DELETE FROM usage_buckets WHERE owner_id=? AND kind=? AND id=?`, owner, args.Kind, args.BucketID)
		case "assign":
			change, err = assignUsageMembers(ctx, tx, owner, authorized, args)
		}
		if err != nil {
			return err
		}
		result.Updated, err = change.RowsAffected()
		if err != nil || args.Action != "assign" || args.BucketID == "" {
			return err
		}
		// Moving members into a published bucket must not smuggle in
		// sources that their owners did not allow to be republished.
		u, d, err := nominatedTx(ctx, tx, owner, args.Kind, args.BucketID)
		if err != nil || !u && !d {
			return err
		}
		if n, err := blockedMembersTx(ctx, tx, owner, args.Kind, args.BucketID); err != nil {
			return err
		} else if n > 0 {
			return &NominationRefusal{Members: n}
		}
		return nil
	})
	return result, err
}

func assignUsageMembers(ctx context.Context, tx *sql.Tx, owner string, workers []ops.WorkerInfo, args ops.UsageBucketSetArgs) (sql.Result, error) {
	visible, err := usageVisibleJSON(workers)
	if err != nil {
		return nil, err
	}
	members, err := json.Marshal(args.Members)
	if err != nil {
		return nil, err
	}
	selectedWorkers, err := json.Marshal(args.WorkerIDs)
	if err != nil {
		return nil, err
	}
	source := "SELECT s.id member_id,s.worker_id FROM usage_sessions s JOIN visible v ON v.id=s.worker_id"
	if args.Kind == "provider" {
		source = "SELECT a.account_id member_id,a.worker_id FROM usage_accounts a JOIN visible v ON v.id=a.worker_id"
	}
	query := usageVisible + ", candidates AS (" + source + `), chosen AS (SELECT DISTINCT member_id FROM candidates WHERE ? OR member_id IN (SELECT value FROM json_each(?)) OR worker_id IN (SELECT value FROM json_each(?))) `
	bind := []any{visible, "", "", args.All, string(members), string(selectedWorkers)}
	if args.BucketID == "" {
		return tx.ExecContext(ctx, query+`DELETE FROM usage_members WHERE owner_id=? AND kind=? AND member_id IN (SELECT member_id FROM chosen)`, append(bind, owner, args.Kind)...)
	}
	return tx.ExecContext(ctx, query+`INSERT INTO usage_members(owner_id,kind,member_id,bucket_id)
	 SELECT ?,?,member_id,? FROM chosen WHERE 1 ON CONFLICT(owner_id,kind,member_id) DO UPDATE SET bucket_id=excluded.bucket_id`, append(bind, owner, args.Kind, args.BucketID)...)
}

func (s *Store) SetUsageAccount(ctx context.Context, owner string, allowed []ops.WorkerInfo, a ops.UsageAccountSetArgs) error {
	if err := a.Validate(); err != nil {
		return err
	}
	visible, err := usageVisibleJSON(allowed)
	if err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, usageVisible+`INSERT INTO usage_account_prefs(owner_id,account_id,label,position)
	SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM usage_accounts a JOIN visible v ON a.worker_id=v.id WHERE a.account_id=?)
	ON CONFLICT(owner_id,account_id) DO UPDATE SET label=excluded.label,position=excluded.position`, visible, "", "", owner, a.AccountID, strings.TrimSpace(a.Label), a.Position, a.AccountID)
	if err != nil {
		return err
	}
	count, err := r.RowsAffected()
	if count == 0 && err == nil {
		return ErrNotFound
	}
	return err
}

// domainBucketAffected reports whether the change touches a bucket that is
// currently published to the whole domain: the bucket itself, or any bucket
// that holds a member the assignment moves or removes.
func domainBucketAffected(ctx context.Context, tx *sql.Tx, owner string, workers []ops.WorkerInfo, args ops.UsageBucketSetArgs) (bool, error) {
	var n int
	if args.Action != "assign" {
		if args.BucketID == "" {
			return false, nil
		}
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_nominations WHERE scope='domain' AND owner_id=? AND kind=? AND bucket_id=?`, owner, args.Kind, args.BucketID).Scan(&n)
		return n > 0, err
	}
	visible, err := usageVisibleJSON(workers)
	if err != nil {
		return false, err
	}
	members, err := json.Marshal(args.Members)
	if err != nil {
		return false, err
	}
	selectedWorkers, err := json.Marshal(args.WorkerIDs)
	if err != nil {
		return false, err
	}
	source := "SELECT s.id member_id,s.worker_id FROM usage_sessions s JOIN visible v ON v.id=s.worker_id"
	if args.Kind == "provider" {
		source = "SELECT a.account_id member_id,a.worker_id FROM usage_accounts a JOIN visible v ON v.id=a.worker_id"
	}
	query := usageVisible + ", candidates AS (" + source + `), chosen AS (SELECT DISTINCT member_id FROM candidates WHERE ? OR member_id IN (SELECT value FROM json_each(?)) OR worker_id IN (SELECT value FROM json_each(?))) 
	 SELECT count(*) FROM usage_nominations n WHERE n.scope='domain' AND n.owner_id=? AND n.kind=? AND (n.bucket_id=? OR EXISTS(
	  SELECT 1 FROM usage_members m WHERE m.owner_id=n.owner_id AND m.kind=n.kind AND m.bucket_id=n.bucket_id AND m.member_id IN (SELECT member_id FROM chosen)))`
	err = tx.QueryRowContext(ctx, query, visible, "", "", args.All, string(members), string(selectedWorkers), owner, args.Kind, args.BucketID).Scan(&n)
	return n > 0, err
}
