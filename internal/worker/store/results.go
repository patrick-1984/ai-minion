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
	// acting admin, API key, source Worker). CallerSession is the calling
	// session the Hub admitted the request for (origin source_session), empty
	// when the Hub named none. PayloadHash is a SHA-256 digest of the canonical
	// arguments of the request, never the arguments themselves. All three are
	// empty in a row written by a release that did not store them; see
	// [ResultRecord.SameRequest] and [resultCallersSchema].
	Caller        string
	CallerSession string
	PayloadHash   string
	// Result is the JSON of the WriteResult. It never holds terminal content.
	Result []byte
	// CreatedAt is set by [Store.PutResult] and returned by [Store.GetResult].
	CreatedAt time.Time
}

// SameRequest reports whether next, the request that arrives now, is the
// request this stored record belongs to: the same op, session and force, the
// same caller, source session and payload. A reused request id that fails it
// is a conflict and must never be answered from the cache (audit F13): the
// cached outcome belongs to another caller or to other input.
//
// Caller, source session and payload are each compared only when both records
// have them. A row written by an older release has none and compares as it
// always did, by op, session and force alone; a request of an older Hub names
// no source session and is compared without it.
func (r ResultRecord) SameRequest(next ResultRecord) bool {
	if r.Op != next.Op || r.Session != next.Session || r.Force != next.Force {
		return false
	}
	for _, p := range [][2]string{{r.Caller, next.Caller}, {r.CallerSession, next.CallerSession}, {r.PayloadHash, next.PayloadHash}} {
		if p[0] != "" && p[1] != "" && p[0] != p[1] {
			return false
		}
	}
	return true
}

// resultCallersSchema keeps who sent a cached write and what it carried
// (audit F13, story W5) beside op_results, in a table of its own and outside
// the numbered migrations. A release that does not know the table ignores it,
// so the schema version stays the one of the release before and a self-update
// that adds it writes no pre-migration backup: a rollback of the update then
// keeps the database, with the receipts of writes the new binary already ran
// and the share changes it made (a restored backup would lose them, and a
// retry of such a write would run again).
//
// A row belongs to the op_results row with the same request_id and
// created_at: a release that does not know the table deletes and inserts
// op_results rows without touching it, and a row it leaves behind whose
// created_at differs describes an earlier request and is not read. created_at
// also prunes it with op_results. Every statement is idempotent; a later
// numbered migration may adopt the table with the same IF NOT EXISTS form.
const resultCallersSchema = `
CREATE TABLE IF NOT EXISTS op_result_callers (
    request_id     TEXT    PRIMARY KEY CHECK (length(request_id) > 0),
    created_at     INTEGER NOT NULL,
    caller         TEXT    NOT NULL,
    source_session TEXT    NOT NULL,
    payload_hash   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS op_result_callers_created_at ON op_result_callers (created_at);`

// ensureResultCallers creates op_result_callers ([resultCallersSchema]) in a
// database that has op_results. A database without op_results (a test that
// replaces the Worker migrations) is left alone.
func ensureResultCallers(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'op_results'`).Scan(&n); err != nil {
		return fmt.Errorf("store: result cache callers: %w", err)
	}
	if n == 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, resultCallersSchema); err != nil {
		return fmt.Errorf("store: result cache callers: %w", err)
	}
	return nil
}

// pruneResults deletes the cache rows older than the TTL at now.
func pruneResults(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	cut := now.Add(-ResultTTL).UnixMilli()
	res, err := tx.ExecContext(ctx, `DELETE FROM op_results WHERE created_at <= ?`, cut)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM op_result_callers WHERE created_at <= ?`, cut); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// putCaller stores the caller members of rec for the op_results row of its
// request id, with that row's created_at.
func putCaller(ctx context.Context, tx *sql.Tx, rec ResultRecord) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO op_result_callers (request_id, created_at, caller, source_session, payload_hash)
		 SELECT request_id, created_at, ?, ?, ? FROM op_results WHERE request_id = ?
		 ON CONFLICT (request_id) DO UPDATE SET
		   created_at = excluded.created_at, caller = excluded.caller,
		   source_session = excluded.source_session, payload_hash = excluded.payload_hash`,
		rec.Caller, rec.CallerSession, rec.PayloadHash, rec.RequestID)
	return err
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
	if _, err := pruneResults(ctx, tx, now); err != nil {
		return fmt.Errorf("store: put result: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO op_results (request_id, op, session_name, force, result, created_at)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (request_id) DO NOTHING`,
		rec.RequestID, rec.Op, rec.Session, boolInt(rec.Force), string(rec.Result), now.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: put result: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: put result: %w", err)
	} else if n == 1 {
		// Only the request that inserted the row names its caller; a second
		// Put for a live id leaves the first caller.
		if err := putCaller(ctx, tx, rec); err != nil {
			return fmt.Errorf("store: put result: %w", err)
		}
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
	if _, err := pruneResults(ctx, tx, now); err != nil {
		return fmt.Errorf("store: replace result: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO op_results (request_id, op, session_name, force, result, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (request_id) DO UPDATE SET
		   op = excluded.op, session_name = excluded.session_name, force = excluded.force, result = excluded.result`,
		rec.RequestID, rec.Op, rec.Session, boolInt(rec.Force), string(rec.Result), now.UnixMilli()); err != nil {
		return fmt.Errorf("store: replace result: %w", err)
	}
	if err := putCaller(ctx, tx, rec); err != nil {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete result: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`DELETE FROM op_results WHERE request_id = ?`, `DELETE FROM op_result_callers WHERE request_id = ?`} {
		if _, err := tx.ExecContext(ctx, q, requestID); err != nil {
			return fmt.Errorf("store: delete result: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete result: %w", err)
	}
	return nil
}

// GetResult returns the cached result of a request id. An entry older than
// [ResultTTL] counts as absent even before it is pruned. The caller members
// are those stored for this very row (same created_at), empty otherwise.
func (s *Store) GetResult(ctx context.Context, requestID string) (ResultRecord, bool, error) {
	var (
		rec     ResultRecord
		force   int
		result  string
		created int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT r.request_id, r.op, r.session_name, r.force, r.result, r.created_at,
		        coalesce(c.caller, ''), coalesce(c.source_session, ''), coalesce(c.payload_hash, '')
		 FROM op_results r
		 LEFT JOIN op_result_callers c ON c.request_id = r.request_id AND c.created_at = r.created_at
		 WHERE r.request_id = ? AND r.created_at > ?`,
		requestID, s.clock.now().Add(-ResultTTL).UnixMilli()).
		Scan(&rec.RequestID, &rec.Op, &rec.Session, &force, &result, &created, &rec.Caller, &rec.CallerSession, &rec.PayloadHash)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: prune results: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := pruneResults(ctx, tx, s.clock.now())
	if err != nil {
		return 0, fmt.Errorf("store: prune results: %w", err)
	}
	if err := tx.Commit(); err != nil {
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
