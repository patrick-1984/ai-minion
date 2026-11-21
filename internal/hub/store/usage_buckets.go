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
	return out, rows.Err()
}

func (s *Store) SetUsageBucket(ctx context.Context, owner string, authorized []ops.WorkerInfo, args ops.UsageBucketSetArgs) (ops.UsageBucketSetResult, error) {
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
		var change sql.Result
		var err error
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
		return err
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
