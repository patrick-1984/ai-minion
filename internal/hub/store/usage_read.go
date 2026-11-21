package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// usageVisible narrows a previously authorized list again at read time.
// Matching both ID and owner fences Worker reassignments between authorization
// and query execution.
const usageVisible = `WITH visible AS (
 SELECT w.id,w.name FROM json_each(?) a JOIN workers w
 ON w.id=json_extract(a.value,'$.id') AND w.owner_id=json_extract(a.value,'$.owner_id')
 WHERE w.approval='approved' AND (?='' OR w.id=?)
) `

type usageVisibleWorker struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

func usageVisibleJSON(workers []ops.WorkerInfo) (string, error) {
	list := make([]usageVisibleWorker, 0, len(workers))
	for _, w := range workers {
		if !w.Pending {
			list = append(list, usageVisibleWorker{w.ID, w.OwnerID})
		}
	}
	b, err := json.Marshal(list)
	return string(b), err
}

// ReadUsage returns at most limit items. An account visible on multiple
// Workers is chosen only among authorized observations, newest first.
// bucketOwner is empty when private organization is outside the caller's rights.
func (s *Store) ReadUsage(ctx context.Context, workers []ops.WorkerInfo, bucketOwner string, args ops.UsageListArgs, now time.Time) (ops.UsageListResult, error) {
	out := ops.UsageListResult{Version: ops.UsageVersion, AsOf: now.UnixMilli()}
	if err := args.Validate(); err != nil {
		return out, err
	}
	visible, err := usageVisibleJSON(workers)
	if err != nil {
		return out, err
	}
	limit := args.Limit
	if limit == 0 {
		limit = 100
	}
	bind := []any{visible, args.WorkerID, args.WorkerID}
	var q string
	switch args.Section {
	case "sessions":
		q = usageVisible + `SELECT s.id,s.worker_id,s.body,s.observed_at,s.ended_at,max(s.observed_at,uw.complete_at),coalesce(m.bucket_id,'')
		 FROM visible v JOIN usage_sessions s ON s.worker_id=v.id JOIN usage_workers uw ON uw.worker_id=s.worker_id
		 LEFT JOIN usage_members m ON m.owner_id=? AND m.kind='session' AND m.member_id=s.id
		 WHERE s.id>? AND (?='' OR coalesce(m.bucket_id,'unassigned')=?) ORDER BY s.id`
		bind = append(bind, bucketOwner, args.After, args.BucketID, args.BucketID)
	case "accounts":
		q = usageVisible + `, ranked AS (
		   SELECT a.*,row_number() OVER (PARTITION BY a.account_id ORDER BY a.observed_at DESC,a.worker_id) rn
		   FROM visible v JOIN usage_accounts a ON a.worker_id=v.id WHERE a.account_id>?
		 ) SELECT a.account_id,a.worker_id,a.body,coalesce(m.bucket_id,'') FROM ranked a
		 LEFT JOIN usage_members m ON m.owner_id=? AND m.kind='provider' AND m.member_id=a.account_id
		 WHERE rn=1 AND (?=0 OR json_extract(a.body,'$.auth_mode')='subscription')
		   AND (?='' OR coalesce(m.bucket_id,'unassigned')=?) ORDER BY a.account_id`
		bind = append(bind, args.After, bucketOwner, args.SubscriptionOnly, args.BucketID, args.BucketID)
	case "workers":
		q = usageVisible + `SELECT v.id,v.name,u.observed_at,u.inventory FROM visible v
		 LEFT JOIN usage_workers u ON u.worker_id=v.id WHERE v.id>? ORDER BY v.id`
		bind = append(bind, args.After)
	}

	rows, err := s.db.QueryContext(ctx, q+" LIMIT ?", append(bind, limit+1)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	last := ""
	for i := 0; rows.Next(); i++ {
		if i == limit {
			out.Next = last
			break
		}
		switch args.Section {
		case "sessions":
			var row ops.UsageSessionRow
			var body string
			var sampleAt, workerAt int64
			var ended *int64
			if err := rows.Scan(&last, &row.WorkerID, &body, &sampleAt, &ended, &workerAt, &row.BucketID); err != nil {
				return out, err
			}
			if err := json.Unmarshal([]byte(body), &row.UsageSession); err != nil {
				return out, err
			}
			row.ObservedAt = sampleAt
			row.StoppedAt = ended
			row.LastSeen = sampleAt
			if ended == nil {
				row.LastSeen = workerAt
			}
			out.Sessions = append(out.Sessions, row)
		case "accounts":
			var row ops.UsageAccountRow
			var body string
			if err := rows.Scan(&last, &row.WorkerID, &body, &row.BucketID); err != nil {
				return out, err
			}
			if err := json.Unmarshal([]byte(body), &row.UsageAccount); err != nil {
				return out, err
			}
			out.Accounts = append(out.Accounts, row)
		case "workers":
			var row ops.UsageWorkerRow
			var inventory sql.NullString
			if err := rows.Scan(&row.ID, &row.Name, &row.ObservedAt, &inventory); err != nil {
				return out, err
			}
			if inventory.Valid {
				if err := json.Unmarshal([]byte(inventory.String), &row.Inventory); err != nil {
					return out, err
				}
			}
			last = row.ID
			out.Workers = append(out.Workers, row)
		}
	}
	return out, rows.Err()
}
