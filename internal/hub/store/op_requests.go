package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OpRequestRetention is how long the answer of a request_id is kept: the
// same ten minutes a Worker caches the result of a write.
const OpRequestRetention = 10 * time.Minute

// OpRequestRow is the stored answer of one request_id of a Hub-scope write
// operation.
type OpRequestRow struct {
	RequestID string
	// Op is the operation name and Actor who asked. A request with the same
	// id and another operation, actor or arguments is not the same request.
	Op    string
	Actor string
	// ArgsSHA256 is the hex SHA-256 of the decoded arguments.
	ArgsSHA256 string
	// ResultJSON is the answer as the API layer encoded it. It never holds a
	// secret: only operations whose result has none are stored.
	ResultJSON string
	CreatedAt  int64 // unix milliseconds
}

// GetOpRequest returns the stored answer of requestID. A row older than
// [OpRequestRetention] at now (unix milliseconds) is not returned: its id is
// free again.
func (s *Store) GetOpRequest(ctx context.Context, requestID string, now int64) (OpRequestRow, bool, error) {
	r := OpRequestRow{RequestID: requestID}
	err := s.db.QueryRowContext(ctx,
		`SELECT op, actor, args_sha256, result_json, created_at FROM op_requests WHERE request_id = ? AND created_at > ?`,
		requestID, opRequestCutoff(now)).Scan(&r.Op, &r.Actor, &r.ArgsSHA256, &r.ResultJSON, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OpRequestRow{}, false, nil
	}
	if err != nil {
		return OpRequestRow{}, false, fmt.Errorf("store: read op request: %w", err)
	}
	return r, true, nil
}

// PutOpRequest stores the answer of a request_id and, in the same
// transaction, deletes every row older than [OpRequestRetention] at
// r.CreatedAt. A row that exists is kept as it is (the first answer of an id
// is the answer); stored reports whether r was written.
func (s *Store) PutOpRequest(ctx context.Context, r OpRequestRow) (stored bool, err error) {
	if r.RequestID == "" || r.Op == "" || r.ArgsSHA256 == "" || r.ResultJSON == "" {
		return false, errors.New("store: an op request needs an id, an operation, an arguments hash and a result")
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM op_requests WHERE created_at <= ?`, opRequestCutoff(r.CreatedAt)); err != nil {
			return fmt.Errorf("store: sweep op requests: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO op_requests (request_id, op, actor, args_sha256, result_json, created_at) VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (request_id) DO NOTHING`,
			r.RequestID, r.Op, r.Actor, r.ArgsSHA256, r.ResultJSON, r.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: put op request: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: put op request: %w", err)
		}
		stored = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return stored, nil
}

// opRequestCutoff is the newest creation time that is no longer kept at now.
func opRequestCutoff(now int64) int64 { return now - OpRequestRetention.Milliseconds() }
