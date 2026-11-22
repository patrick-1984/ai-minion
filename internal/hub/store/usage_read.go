package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
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

const usageRankedAccounts = `, ranked AS (
		   SELECT a.*,row_number() OVER (PARTITION BY a.account_id ORDER BY (a.worker_id=settings.preferred_worker AND (settings.preferred_session='' OR json_extract(a.body,'$.source_session_id')=settings.preferred_session)) DESC,(json_extract(a.body,'$.source')<>'native-account') DESC,a.observed_at DESC,a.worker_id) rn
		   FROM visible v JOIN usage_accounts a ON a.worker_id=v.id LEFT JOIN usage_collectors settings ON settings.account_id=a.account_id
		 )`

// A provider bucket selects direct account membership. A session bucket
// projects only visible sessions onto the corresponding account IDs.
const usageAccountBucketFilter = `(?='' OR coalesce(m.bucket_id,'unassigned')=?
 OR EXISTS(SELECT 1 FROM usage_sessions linked JOIN visible sw ON sw.id=linked.worker_id
 JOIN usage_members sm ON sm.member_id=linked.id AND sm.kind='session' AND sm.owner_id=?
 WHERE json_extract(linked.body,'$.account_id')=a.account_id AND sm.bucket_id=?))`

// accountExpr is always a static column expression selected by this package.
func usageSessionBucketFilter(accountExpr string) string {
	return `(?='' OR coalesce(m.bucket_id,'unassigned')=? OR EXISTS(SELECT 1 FROM usage_members pm
	 WHERE pm.owner_id=? AND pm.kind='provider' AND pm.member_id=` + accountExpr + ` AND pm.bucket_id=?))`
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
	case "overview":
		rows, err := s.db.QueryContext(ctx, usageVisible+usageRankedAccounts+`SELECT a.body FROM ranked a
		 LEFT JOIN usage_members m ON m.owner_id=? AND m.kind='provider' AND m.member_id=a.account_id
		 WHERE rn=1 AND json_extract(a.body,'$.auth_mode')='subscription' AND `+usageAccountBucketFilter, append(bind, bucketOwner, args.BucketID, args.BucketID, bucketOwner, args.BucketID)...)
		if err != nil {
			return out, err
		}
		defer func() { _ = rows.Close() }()
		var all []ops.UsageAccountRow
		for rows.Next() {
			var body string
			var a ops.UsageAccountRow
			if err := rows.Scan(&body); err != nil {
				return out, err
			}
			if err := json.Unmarshal([]byte(body), &a.UsageAccount); err != nil {
				return out, err
			}
			all = append(all, a)
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
		summary := ops.SummarizeUsage(all, now)
		out.Overview = &summary
		return out, nil
	case "sessions":
		q = usageVisible + `SELECT s.id,s.worker_id,s.body,s.observed_at,s.ended_at,max(s.observed_at,uw.complete_at),coalesce(m.bucket_id,'')
		 FROM visible v JOIN usage_sessions s ON s.worker_id=v.id JOIN usage_workers uw ON uw.worker_id=s.worker_id
		 LEFT JOIN usage_members m ON m.owner_id=? AND m.kind='session' AND m.member_id=s.id
		 WHERE s.id>? AND ` + usageSessionBucketFilter(`json_extract(s.body,'$.account_id')`) + ` ORDER BY s.id`
		bind = append(bind, bucketOwner, args.After, args.BucketID, args.BucketID, bucketOwner, args.BucketID)
	case "accounts":
		q = usageVisible + usageRankedAccounts + `SELECT printf('%07d:%s',coalesce(pref.position,0),a.account_id),a.worker_id,a.body,coalesce(m.bucket_id,''),coalesce(c.status,''),coalesce(pw.id,''),coalesce(aw.id,''),
		 CASE WHEN pw.id IS NULL THEN '' ELSE c.preferred_session END,CASE WHEN c.hide_previous_schedule=0 THEN nullif(c.next_due,0) END,c.last_success,EXISTS(SELECT 1 FROM usage_accounts cap JOIN visible cv ON cap.worker_id=cv.id WHERE cap.account_id=a.account_id AND cap.can_poll=1),coalesce(pref.label,''),coalesce(pref.position,0),
		 (SELECT json_group_array(json_object('worker_id',worker,'session_id',sid,'label',label)) FROM (
		  SELECT v2.id worker,'' sid,v2.name label FROM visible v2 JOIN usage_accounts u2 ON u2.worker_id=v2.id WHERE u2.account_id=a.account_id
		  UNION ALL
		  SELECT v3.id,s3.id,v3.name || ' / ' || json_extract(s3.body,'$.name') FROM visible v3 JOIN usage_sessions s3 ON s3.worker_id=v3.id
		  WHERE s3.ended_at IS NULL AND json_extract(s3.body,'$.account_id')=a.account_id ORDER BY label LIMIT 256))
		 FROM ranked a
		 LEFT JOIN usage_collectors c ON c.account_id=a.account_id AND (c.active_worker IS NULL OR c.active_worker IN (SELECT id FROM visible))
		 AND (c.preferred_worker IS NULL OR c.preferred_worker IN (SELECT id FROM visible)) LEFT JOIN visible pw ON c.preferred_worker=pw.id LEFT JOIN visible aw ON c.active_worker=aw.id
		 LEFT JOIN usage_account_prefs pref ON pref.account_id=a.account_id AND pref.owner_id=?
		 LEFT JOIN usage_members m ON m.owner_id=? AND m.kind='provider' AND m.member_id=a.account_id
		 WHERE rn=1 AND printf('%07d:%s',coalesce(pref.position,0),a.account_id)>? AND (?=0 OR json_extract(a.body,'$.auth_mode')='subscription')
		   AND ` + usageAccountBucketFilter + ` ORDER BY coalesce(pref.position,0),a.account_id`
		bind = append(bind, bucketOwner, bucketOwner, args.After, args.SubscriptionOnly, args.BucketID, args.BucketID, bucketOwner, args.BucketID)
	case "events":
		q = usageVisible + `, events AS (SELECT e.*,printf('%019d:%s:%019d',e.observed_at,e.worker_id,e.id) cursor FROM visible v JOIN usage_events e ON e.worker_id=v.id)
		  SELECT e.cursor,e.worker_id,e.body FROM events e LEFT JOIN usage_members m ON m.kind='session' AND m.owner_id=? AND m.member_id=e.session_id
		  WHERE (?='' OR e.cursor<?) AND ` + usageSessionBucketFilter(`json_extract(e.body,'$.account_id')`) + ` ORDER BY cursor DESC`
		bind = append(bind, bucketOwner, args.After, args.After, args.BucketID, args.BucketID, bucketOwner, args.BucketID)
	case "history":
		q = usageVisible + `, samples AS (SELECT h.*,s.worker_id,printf('%019d:%s',h.hour,h.session_id) cursor FROM visible v JOIN usage_sessions s ON s.worker_id=v.id JOIN usage_hours h ON h.session_id=s.id)
		  SELECT h.cursor,h.session_id,h.worker_id,h.hour*3600000,h.observed_at,h.counters FROM samples h
		  LEFT JOIN usage_members m ON m.kind='session' AND m.owner_id=? AND m.member_id=h.session_id
		  WHERE (?='' OR h.cursor<?) AND ` + usageSessionBucketFilter(`h.account_id`) + ` ORDER BY cursor DESC`
		bind = append(bind, bucketOwner, args.After, args.After, args.BucketID, args.BucketID, bucketOwner, args.BucketID)
	case "workers":
		q = usageVisible + `SELECT v.id,v.name,nullif(u.observed_at,0),u.inventory,coalesce(u.status,'waiting'),u.inventory_at FROM visible v
		 LEFT JOIN usage_workers u ON u.worker_id=v.id WHERE v.id>? ORDER BY v.id`
		bind = append(bind, args.After)
	}

	rows, err := s.db.QueryContext(ctx, q+" LIMIT ?", append(bind, limit+1)...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	last := ""
	size := 0
page:
	for i := 0; rows.Next(); i++ {
		if i == limit {
			out.Next = last
			break
		}
		previous := last
		fits := func(row any) bool {
			b, err := json.Marshal(row)
			if err != nil || (i > 0 && size+len(b) > 700<<10) {
				out.Next = previous
				return false
			}
			size += len(b) + 1
			return true
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
			if ended != nil {
				row.StopSource = "native"
				if row.StoppedAt == nil {
					row.StopSource = "first_absent_scan"
				}
			}
			row.StoppedAt = ended
			row.LastSeen = sampleAt
			if ended == nil {
				row.LastSeen = workerAt
			}
			if row.DurationMS != nil {
				row.DurationSource = "native"
			} else if row.StartedAt != nil {
				stop := row.LastSeen
				if ended != nil {
					stop = *ended
				}
				if stop >= *row.StartedAt {
					v := stop - *row.StartedAt
					row.DurationMS = &v
					row.DurationSource = "observed_host_lifetime"
				}
			}
			if !fits(row) {
				break page
			}
			out.Sessions = append(out.Sessions, row)
		case "accounts":
			var row ops.UsageAccountRow
			var body string
			var collector ops.UsageCollector
			var sources, privateLabel string
			if err := rows.Scan(&last, &row.WorkerID, &body, &row.BucketID, &collector.Status, &collector.PreferredWorker, &collector.ActiveWorker, &collector.PreferredSession, &collector.NextDue, &collector.LastSuccess, &collector.CanStart, &privateLabel, &row.Position, &sources); err != nil {
				return out, err
			}
			if err := json.Unmarshal([]byte(body), &row.UsageAccount); err != nil {
				return out, err
			}
			if err := json.Unmarshal([]byte(sources), &row.Sources); err != nil {
				return out, err
			}
			if privateLabel != "" {
				row.Alias = privateLabel
				row.Label = privateLabel
			}
			if collector.Status != "" {
				collector.Mode = "passive_events"
				if collector.CanStart && collector.PreferredSession == "" {
					collector.Mode = "native_background"
				}
				row.Collector = &collector
			}
			if !fits(row) {
				break page
			}
			out.Accounts = append(out.Accounts, row)
		case "events":
			var row ops.UsageEventRow
			var b string
			if err := rows.Scan(&last, &row.WorkerID, &b); err != nil {
				return out, err
			}
			if err := json.Unmarshal([]byte(b), &row.UsageEvent); err != nil {
				return out, err
			}
			if !fits(row) {
				break page
			}
			out.Events = append(out.Events, row)
		case "history":
			var row ops.UsageHistoryRow
			var b string
			if err := rows.Scan(&last, &row.SessionID, &row.WorkerID, &row.Hour, &row.ObservedAt, &b); err != nil {
				return out, err
			}
			if err := json.Unmarshal([]byte(b), &row.Counters); err != nil {
				return out, err
			}
			if !fits(row) {
				break page
			}
			out.History = append(out.History, row)
		case "workers":
			var row ops.UsageWorkerRow
			var inventory sql.NullString
			if err := rows.Scan(&row.ID, &row.Name, &row.ObservedAt, &inventory, &row.Status, &row.InventoryAt); err != nil {
				return out, err
			}
			if inventory.Valid {
				if err := json.Unmarshal([]byte(inventory.String), &row.Inventory); err != nil {
					return out, err
				}
			}
			if row.Status == "unavailable" || row.Status == "registration_required" {
				row.LastIssue = row.Status
			}
			for _, w := range workers {
				if w.ID == row.ID {
					row.Online = w.Online
					if !w.Online {
						row.Status = "offline"
					} else if !slices.Contains(w.Ops, ops.OpWorkerUsage) {
						row.Status = "unsupported"
					}
					break
				}
			}
			last = row.ID
			if !fits(row) {
				break page
			}
			out.Workers = append(out.Workers, row)
		}
	}
	return out, rows.Err()
}

// usageQuerier is the read side shared by the pool and a transaction, so one
// document can be built from one snapshot.
type usageQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// UsageSlotSpec is one slot a Worker's owner receives: a nominated bucket and
// the publisher whose publication rights bound what it may include.
type UsageSlotSpec struct {
	Scope, Kind, BucketID, Label, Publisher string
}

// usageRecipientOwner is the SQL condition that the Worker owner ?1 may
// receive read-back: an active account that still holds a grant.
const usageRecipientOwner = `EXISTS(SELECT 1 FROM users ro WHERE ro.id=?1 AND ro.active=1
 AND EXISTS(SELECT 1 FROM user_roles rr WHERE rr.user_id=ro.id))`

// ReadbackRecipientEligible returns the Worker's current owner and whether the
// Worker may receive the nominated usage document right now: it is approved
// and its owner is an active account that still holds a grant (user or admin).
// The owner is returned even when the Worker is not eligible, so that delivery
// can tell "lost eligibility" (send the clear) from "reassigned" (end the
// connection). An unknown Worker has an empty owner. Delivery calls it before
// it builds and again before it sends each document.
func (s *Store) ReadbackRecipientEligible(ctx context.Context, workerID string) (owner string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT w.owner_id, w.approval='approved'
	 AND EXISTS(SELECT 1 FROM users ro WHERE ro.id=w.owner_id AND ro.active=1
	  AND EXISTS(SELECT 1 FROM user_roles rr WHERE rr.user_id=ro.id))
	 FROM workers w WHERE w.id=?1`, workerID).Scan(&owner, &ok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return owner, ok, nil
}

// UsageReadbackSlots lists the effective slots for the Workers of owner, the
// domain pair first, each kind in order session, provider. The recipient must
// be an active account with a grant. A domain slot counts only while its
// publisher is an active administrator; a user slot only while its owner is
// active.
func (s *Store) UsageReadbackSlots(ctx context.Context, owner string) ([]UsageSlotSpec, error) {
	return usageReadbackSlots(ctx, s.db, owner)
}

func usageReadbackSlots(ctx context.Context, q usageQuerier, owner string) ([]UsageSlotSpec, error) {
	rows, err := q.QueryContext(ctx, `SELECT n.scope,n.kind,n.bucket_id,b.label,n.owner_id
	 FROM usage_nominations n JOIN usage_buckets b ON b.owner_id=n.owner_id AND b.kind=n.kind AND b.id=n.bucket_id
	 JOIN users u ON u.id=n.owner_id AND u.active=1
	 WHERE `+usageRecipientOwner+` AND ((n.scope='user' AND n.owner_id=?1)
	    OR (n.scope='domain' AND EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role='admin')))
	 ORDER BY CASE n.scope WHEN 'domain' THEN 0 ELSE 1 END, CASE n.kind WHEN 'session' THEN 0 ELSE 1 END`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageSlotSpec
	for rows.Next() {
		var sp UsageSlotSpec
		if err := rows.Scan(&sp.Scope, &sp.Kind, &sp.BucketID, &sp.Label, &sp.Publisher); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// NominatedUsage is the authorized raw material of one slot.
type NominatedUsage struct {
	Accounts []ops.UsageAccount            // provider slot: best observation per account
	Sessions []ops.UsageSessionObservation // session slot
	Linked   []ops.UsageAccount            // session slot: accounts the sessions are linked to
}

// ReadbackSlot is one slot with its publishable sources and, when asked for,
// its raw data, all read from one snapshot.
type ReadbackSlot struct {
	UsageSlotSpec
	Sources []UsagePublishableSource
	Data    NominatedUsage
}

// UsageReadbackSnapshot reads the slots of owner, their publishable sources
// and (withData) the raw observations in ONE transaction, so consent
// withdrawn between two reads cannot let a later observation into a document
// built from an earlier answer. visible maps a publisher to the Worker ids the
// publisher may view now (the ordinary Usage visibility, decided by the
// caller). A publisher without an entry may publish only the sources of
// their own Workers. A source owned by somebody else is publishable only when
// its owner allowed republishing AND the publisher may still view its Worker.
func (s *Store) UsageReadbackSnapshot(ctx context.Context, owner string, visible map[string][]string, withData bool) ([]ReadbackSlot, error) {
	var out []ReadbackSlot
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		out = nil
		specs, err := usageReadbackSlots(ctx, tx, owner)
		if err != nil {
			return err
		}
		for _, sp := range specs {
			src, err := usagePublishableSources(ctx, tx, sp.Publisher, sp.Kind, sp.BucketID, true, visible[sp.Publisher])
			if err != nil {
				return err
			}
			slot := ReadbackSlot{UsageSlotSpec: sp, Sources: src}
			if withData {
				if slot.Data, err = readNominatedUsage(ctx, tx, sp.Kind, src); err != nil {
					return err
				}
			}
			out = append(out, slot)
		}
		return nil
	})
	return out, err
}

type usageSourcePair struct {
	M string `json:"m"`
	W string `json:"w"`
}

func usagePairsJSON(pairs []usageSourcePair) (string, error) {
	b, err := json.Marshal(pairs)
	return string(b), err
}

// readUsageAccounts returns the best observation of each requested
// (account, Worker) pair on an approved Worker. Only the listed pairs are
// read, so what was not authorized for publication cannot influence which
// observation wins. Among the listed Workers a row that reports windows beats
// a discovery-only row, then the collector the owner designated wins, then a
// non-native observation, then the newest.
func readUsageAccounts(ctx context.Context, q usageQuerier, pairs []usageSourcePair) ([]ops.UsageAccount, error) {
	arg, err := usagePairsJSON(pairs)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT a.account_id,a.observed_at,a.body FROM json_each(?) p
	 JOIN workers w ON w.id=json_extract(p.value,'$.w') AND w.approval='approved'
	 JOIN usage_accounts a ON a.worker_id=w.id AND a.account_id=json_extract(p.value,'$.m')
	 LEFT JOIN usage_collectors settings ON settings.account_id=a.account_id
	 ORDER BY a.account_id,
	  (json_array_length(a.body,'$.windows')>0) DESC,
	  (a.worker_id=settings.preferred_worker AND (settings.preferred_session='' OR json_extract(a.body,'$.source_session_id')=settings.preferred_session)) DESC,
	  (json_extract(a.body,'$.source')<>'native-account') DESC,
	  a.observed_at DESC,a.worker_id`, arg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ops.UsageAccount
	last := ""
	for rows.Next() {
		var id, body string
		var observed int64
		if err := rows.Scan(&id, &observed, &body); err != nil {
			return nil, err
		}
		if id == last {
			continue
		}
		last = id
		var a ops.UsageAccount
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			return nil, err
		}
		a.ObservedAt = observed
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReadNominatedUsage reads the raw rows of one slot from its publishable
// sources (UsagePublishableSources, applied before anything is deduplicated
// or aggregated). A session slot also reads the subscription accounts its
// sessions are linked to, from the same Workers. Prefer UsageReadbackSnapshot,
// which reads the sources and the rows in one transaction.
func (s *Store) ReadNominatedUsage(ctx context.Context, kind string, sources []UsagePublishableSource) (NominatedUsage, error) {
	return readNominatedUsage(ctx, s.db, kind, sources)
}

func readNominatedUsage(ctx context.Context, q usageQuerier, kind string, sources []UsagePublishableSource) (NominatedUsage, error) {
	var out NominatedUsage
	pairs := make([]usageSourcePair, len(sources))
	for i, p := range sources {
		pairs[i] = usageSourcePair{p.MemberID, p.WorkerID}
	}
	if kind == "provider" {
		var err error
		out.Accounts, err = readUsageAccounts(ctx, q, pairs)
		return out, err
	}
	arg, err := usagePairsJSON(pairs)
	if err != nil {
		return out, err
	}
	rows, err := q.QueryContext(ctx, `SELECT s.worker_id,s.observed_at,s.ended_at,s.body FROM json_each(?) p
	 JOIN workers w ON w.id=json_extract(p.value,'$.w') AND w.approval='approved'
	 JOIN usage_sessions s ON s.worker_id=w.id AND s.id=json_extract(p.value,'$.m') ORDER BY s.id`, arg)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	var links []usageSourcePair
	for rows.Next() {
		var worker, body string
		var observed int64
		var ended *int64
		if err := rows.Scan(&worker, &observed, &ended, &body); err != nil {
			return out, err
		}
		var o ops.UsageSessionObservation
		if err := json.Unmarshal([]byte(body), &o.UsageSession); err != nil {
			return out, err
		}
		o.ObservedAt, o.Ended = observed, ended != nil
		out.Sessions = append(out.Sessions, o)
		if o.AccountID != "" {
			links = append(links, usageSourcePair{o.AccountID, worker})
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	_ = rows.Close()
	if len(links) > 0 {
		out.Linked, err = readUsageAccounts(ctx, q, links)
	}
	return out, err
}
