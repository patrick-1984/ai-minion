package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResultTTL is how long a write result stays cached for duplicate request ids
// (AD-15).
const ResultTTL = 10 * time.Minute

// ResultRecord is one cached write result.
type ResultRecord struct {
	// RequestID is the caller's UUIDv7; it is the key.
	RequestID string
	// Op, Session and Force describe the request the result belongs to, so
	// that a reused request id with a different request is detected.
	Op      string
	Session string
	Force   bool
	// Caller names who sent the request: "local" for a caller on this host,
	// otherwise the identity members of the Hub-derived origin (kind, user,
	// acting admin, API key, source Worker). PayloadHash is a SHA-256 digest of
	// the canonical arguments of the request, never the arguments themselves.
	// Both are empty in a row written before they existed (migration 0015);
	// see [ResultRecord.SameRequest].
	Caller      string
	PayloadHash string
	// Result is the JSON of the WriteResult. It never holds terminal content.
	Result []byte
	// CreatedAt is set by [Store.PutResult] and returned by [Store.GetResult].
	CreatedAt time.Time
}

// SameRequest reports whether next, the request that arrives now, is the
// request this stored record belongs to: the same op, session and force, the
// same caller and the same payload. A reused request id that fails it is a
// conflict and must never be answered from the cache (audit F13): the cached
// outcome belongs to another caller or to other input.
//
// Caller and payload are compared only when both records have them. A row
// written before migration 0015 has neither and compares as it always did, by
// op, session and force alone.
func (r ResultRecord) SameRequest(next ResultRecord) bool {
	if r.Op != next.Op || r.Session != next.Session || r.Force != next.Force {
		return false
	}
	if r.Caller != "" && next.Caller != "" && r.Caller != next.Caller {
		return false
	}
	if r.PayloadHash != "" && next.PayloadHash != "" && r.PayloadHash != next.PayloadHash {
		return false
	}
	return true
}

// PutResult stores the result of a write under its request id. The first
// result for an id wins: a second Put for a live id changes nothing. Entries
// older than [ResultTTL] are pruned in the same transaction, so an expired id
// can be reused.
func (s *Store) PutResult(ctx context.Context, rec ResultRecord) error {
	if rec.RequestID == "" {
		return errors.New("store: result record without request id")
	}
	now := s.clock.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: put result: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM op_results WHERE created_at <= ?`, now.Add(-ResultTTL).UnixMilli()); err != nil {
		return fmt.Errorf("store: put result: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO op_results (request_id, op, session_name, force, result, created_at, caller, payload_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (request_id) DO NOTHING`,
		rec.RequestID, rec.Op, rec.Session, boolInt(rec.Force), string(rec.Result), now.UnixMilli(), rec.Caller, rec.PayloadHash); err != nil {
		return fmt.Errorf("store: put result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: put result: %w", err)
	}
	return nil
}

// ReplaceResult sets the result of a request id to rec, whatever an earlier
// Put stored. It is how a record written before a side effect (a result that
// says "unknown") becomes the completed result afterwards. The creation time
// of a live entry is kept, so the replacement does not extend the cache
// lifetime; an id that was pruned meanwhile is inserted again.
func (s *Store) ReplaceResult(ctx context.Context, rec ResultRecord) error {
	if rec.RequestID == "" {
		return errors.New("store: result record without request id")
	}
	now := s.clock.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace result: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM op_results WHERE created_at <= ?`, now.Add(-ResultTTL).UnixMilli()); err != nil {
		return fmt.Errorf("store: replace result: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO op_results (request_id, op, session_name, force, result, created_at, caller, payload_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (request_id) DO UPDATE SET
		   op = excluded.op, session_name = excluded.session_name, force = excluded.force, result = excluded.result,
		   caller = excluded.caller, payload_hash = excluded.payload_hash`,
		rec.RequestID, rec.Op, rec.Session, boolInt(rec.Force), string(rec.Result), now.UnixMilli(), rec.Caller, rec.PayloadHash); err != nil {
		return fmt.Errorf("store: replace result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace result: %w", err)
	}
	return nil
}

// DeleteResult removes the entry of a request id, if there is one. It undoes
// a record whose request turned out to have no effect, so that the caller may
// repeat it.
func (s *Store) DeleteResult(ctx context.Context, requestID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM op_results WHERE request_id = ?`, requestID); err != nil {
		return fmt.Errorf("store: delete result: %w", err)
	}
	return nil
}

// GetResult returns the cached result of a request id. An entry older than
// [ResultTTL] counts as absent even before it is pruned.
func (s *Store) GetResult(ctx context.Context, requestID string) (ResultRecord, bool, error) {
	var (
		rec     ResultRecord
		force   int
		result  string
		created int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT request_id, op, session_name, force, result, created_at, caller, payload_hash
		 FROM op_results WHERE request_id = ? AND created_at > ?`,
		requestID, s.clock.now().Add(-ResultTTL).UnixMilli()).
		Scan(&rec.RequestID, &rec.Op, &rec.Session, &force, &result, &created, &rec.Caller, &rec.PayloadHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ResultRecord{}, false, nil
	}
	if err != nil {
		return ResultRecord{}, false, fmt.Errorf("store: get result: %w", err)
	}
	rec.Force = force != 0
	rec.Result = []byte(result)
	rec.CreatedAt = time.UnixMilli(created).UTC()
	return rec, true, nil
}

// PruneResults deletes the entries older than [ResultTTL] and returns how many
// it removed. PutResult prunes too; this is for an idle Worker.
func (s *Store) PruneResults(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM op_results WHERE created_at <= ?`, s.clock.now().Add(-ResultTTL).UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune results: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune results: %w", err)
	}
	return n, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
