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
	// Source is the session a brokered call came from ("<worker_id>:<session>")
	// or the session a control change names; GrantID the grant that allowed
	// the call; Detail a closed list of k=v pairs. All may be empty.
	Source  string
	GrantID string
	Detail  string
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
	// GrantID keeps only the rows of this grant: rows whose grant_id is this
	// id, or names it as one complete member of a comma-separated list (a
	// brokered write that several overlapping grants authorized together).
	GrantID string
	From    *int64    // include rows with ts >= From (unix ms)
	To      *int64    // include rows with ts < To (unix ms)
	After   *AuditPos // only rows strictly older than this position
	Limit   int       // maximum rows; must be positive
}

// InsertAudit writes one audit row. ex is the caller's open transaction, or
// nil to write in a transaction of its own.
func (s *Store) InsertAudit(ctx context.Context, ex Execer, row AuditRow) error {
	if ex == nil {
		ex = s.db
	}
	// The row is reported below, so it is exported already (0036); a row
	// that SQL writes itself keeps exported = 0 for forwardSQLAudit.
	query := `INSERT INTO audit_events (id, ts, actor, acting_as, action, target, outcome, source, grant_id, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if s.auditExport {
		query = `INSERT INTO audit_events (id, ts, actor, acting_as, action, target, outcome, source, grant_id, detail, exported) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`
	}
	_, err := ex.ExecContext(ctx, query,
		row.ID, row.TS, row.Actor, row.ActingAs, row.Action, row.Target, row.Outcome, row.Source, row.GrantID, row.Detail)
	if err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	if obs := s.auditObs.Load(); obs != nil {
		AfterCommit(ex, func() { (*obs)(ctx, row) })
	} else {
		AfterCommit(ex, func() { s.holdEarlyAudit(ctx, row) })
	}
	return nil
}

// auditExportBatch bounds the rows one forwarding step reads and marks.
const auditExportBatch = 256

// auditExportColumn reports whether audit_events has the exported column of
// migration 0036 (a database with test migrations may not).
func auditExportColumn(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('audit_events') WHERE name = 'exported'`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: read audit schema: %w", err)
	}
	return n == 1, nil
}

// forwardSQLAudit hands the committed audit rows that did not pass
// [Store.InsertAudit] (exported = 0: the grant triggers of 0034, fired by a
// demotion, a Worker reassignment or removal) to the audit observer, oldest
// first, so they reach the log sinks like every other row. [Store.Tx] calls
// it after each commit and [Store.SetAuditObserver] when the observer is set,
// so a row written before there was an observer (the start-up reconciliation
// inside [Open], or a run that stopped first) waits in the table until then.
//
// Each row is marked exported in a transaction before the observer sees it,
// and only a row whose mark changed it is passed on: one row, one export,
// whatever the number of callers. A caller that finds a forwarding running
// leaves its rows to that one, which runs once more.
func (s *Store) forwardSQLAudit() {
	if !s.auditExport || s.auditObs.Load() == nil {
		return
	}
	s.fwdAgain.Store(true)
	for s.fwdMu.TryLock() {
		for s.fwdAgain.Swap(false) {
			for s.forwardSQLAuditBatch() {
			}
		}
		s.fwdMu.Unlock()
		if !s.fwdAgain.Load() {
			return
		}
	}
}

// forwardSQLAuditBatch forwards up to auditExportBatch rows and reports
// whether more may be waiting. An error leaves the rows for the next call.
func (s *Store) forwardSQLAuditBatch() bool {
	obs := s.auditObs.Load()
	if obs == nil {
		return false
	}
	ctx := context.Background()
	var (
		out  []AuditRow
		more bool
	)
	err := Tx(ctx, s.db, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, `SELECT rowid, id, ts, actor, acting_as, action, target, outcome, source, grant_id, detail
			FROM audit_events WHERE exported = 0 ORDER BY rowid LIMIT ?`, auditExportBatch)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var (
				rid int64
				r   AuditRow
			)
			if err := rows.Scan(&rid, &r.ID, &r.TS, &r.Actor, &r.ActingAs, &r.Action, &r.Target, &r.Outcome, &r.Source, &r.GrantID, &r.Detail); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, rid)
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		more = len(ids) == auditExportBatch
		kept := out[:0]
		for i, rid := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE audit_events SET exported = 1 WHERE rowid = ? AND exported = 0`, rid)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				kept = append(kept, out[i])
			}
		}
		out = kept
		return nil
	})
	if err != nil {
		return false
	}
	for _, r := range out {
		(*obs)(ctx, r)
	}
	return more
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
		if err := rows.Scan(&r.ID, &r.TS, &r.Actor, &r.ActingAs, &r.Action, &r.Target, &r.Outcome, &r.Source, &r.GrantID, &r.Detail); err != nil {
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
	if q.GrantID != "" {
		// One complete id among the comma-separated ids of the row (a
		// write that overlapping grants authorized names each of them), never
		// a part of an id. The partial index audit_events_grant_ts (0033)
		// still bounds the scan to rows that name a grant.
		where = append(where, "grant_id <> '' AND instr(',' || grant_id || ',', ',' || ? || ',') > 0")
		args = append(args, q.GrantID)
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
	query := `SELECT id, ts, actor, acting_as, action, target, outcome, source, grant_id, detail FROM audit_events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY ts DESC, id DESC LIMIT ?"
	args = append(args, q.Limit)
	return query, args
}
