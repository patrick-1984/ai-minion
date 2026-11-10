package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Execer is what both *sql.DB and *sql.Tx offer for a statement. Domain
// packages hand their open transaction to [Store.InsertAudit] through it, so
// an audit row commits or rolls back with the change it describes (AD-16).
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// AuditRow is one row of the audit_events table. It holds no content.
type AuditRow struct {
	ID       string // UUIDv7
	TS       int64  // unix milliseconds
	Actor    string
	ActingAs string // empty when the actor did not act as someone else
	Action   string
	Target   string
	Outcome  string
}

// AuditPos is a position in the newest-first order: the (ts, id) of a row.
type AuditPos struct {
	TS int64
	ID string
}

// AuditQuery selects audit rows, newest first (ts descending, then id
// descending). Empty strings and nil pointers do not filter.
type AuditQuery struct {
	Actor  string
	Action string
	From   *int64    // include rows with ts >= From (unix ms)
	To     *int64    // include rows with ts < To (unix ms)
	After  *AuditPos // only rows strictly older than this position
	Limit  int       // maximum rows; must be positive
}

// InsertAudit writes one audit row. ex is the caller's open transaction, or
// nil to write in a transaction of its own.
func (s *Store) InsertAudit(ctx context.Context, ex Execer, row AuditRow) error {
	if ex == nil {
		ex = s.db
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO audit_events (id, ts, actor, acting_as, action, target, outcome) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.TS, row.Actor, row.ActingAs, row.Action, row.Target, row.Outcome)
	if err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	return nil
}

// ListAudit returns up to q.Limit rows matching q, newest first.
func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditRow, error) {
	if q.Limit <= 0 {
		return nil, fmt.Errorf("store: list audit events: limit must be positive")
	}
	query, args := auditSelect(q)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Actor, &r.ActingAs, &r.Action, &r.Target, &r.Outcome); err != nil {
			return nil, fmt.Errorf("store: list audit events: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit events: %w", err)
	}
	return out, nil
}

// auditSelect builds the statement and bound arguments for q.
func auditSelect(q AuditQuery) (string, []any) {
	var (
		where []string
		args  []any
	)
	if q.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, q.Actor)
	}
	if q.Action != "" {
		where = append(where, "action = ?")
		args = append(args, q.Action)
	}
	if q.From != nil {
		where = append(where, "ts >= ?")
		args = append(args, *q.From)
	}
	if q.To != nil {
		where = append(where, "ts < ?")
		args = append(args, *q.To)
	}
	if q.After != nil {
		// A row-value comparison lets SQLite seek the (ts, id) index; the
		// equivalent OR predicate degrades to a scan of all newer rows.
		where = append(where, "(ts, id) < (?, ?)")
		args = append(args, q.After.TS, q.After.ID)
	}
	query := `SELECT id, ts, actor, acting_as, action, target, outcome FROM audit_events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY ts DESC, id DESC LIMIT ?"
	args = append(args, q.Limit)
	return query, args
}
