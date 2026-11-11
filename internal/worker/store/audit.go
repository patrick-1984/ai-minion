package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AuditCap is the number of local audit rows kept. AppendAudit drops the
// oldest rows beyond it, so the table cannot grow without bound.
const AuditCap = 100000

// AuditRecord is one row of the Worker's local audit: which write op ran
// against which target and how it ended (AD-20). It never holds terminal
// content, message text, keys or secrets.
type AuditRecord struct {
	ID int64
	// TS is set by [Store.AppendAudit] from the store clock.
	TS time.Time
	// Op is the op name, for example "send_text".
	Op string
	// Target is the log-safe target "worker_id:session[:window.pane]".
	Target string
	// Outcome is an AD-15 outcome, or "error" when the op ended in an API
	// error such as forbidden.
	Outcome string
	// Reason is the reason code (or the API error code); empty on delivery.
	Reason string
	// Forced reports that force=true overrode a FORCE guard.
	Forced    bool
	RequestID string
	// Origin is who made the request: the JSON origin the Hub derived, "local"
	// for a local caller, or "missing" or "malformed" for a refused request
	// whose origin was not usable (F-01). Never terminal content.
	Origin string
}

// AppendAudit adds a row to the local audit and trims the table to
// [AuditCap] rows.
func (s *Store) AppendAudit(ctx context.Context, rec AuditRecord) error {
	if rec.Op == "" || rec.Outcome == "" {
		return errors.New("store: audit record needs an op and an outcome")
	}
	now := s.clock.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO local_audit (ts, op, target, outcome, reason, forced, request_id, origin)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		now, rec.Op, rec.Target, rec.Outcome, rec.Reason, boolInt(rec.Forced), rec.RequestID, rec.Origin).Scan(&id); err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}
	if id > AuditCap {
		if _, err := tx.ExecContext(ctx, `DELETE FROM local_audit WHERE id <= ?`, id-AuditCap); err != nil {
			return fmt.Errorf("store: append audit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}
	return nil
}

// ListAudit returns up to limit rows, newest first. A limit below 1 means 100.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditRecord, error) {
	if limit < 1 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, op, target, outcome, reason, forced, request_id, origin
		 FROM local_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []AuditRecord{}
	for rows.Next() {
		var (
			r      AuditRecord
			ts     int64
			forced int
		)
		if err := rows.Scan(&r.ID, &ts, &r.Op, &r.Target, &r.Outcome, &r.Reason, &forced, &r.RequestID, &r.Origin); err != nil {
			return nil, fmt.Errorf("store: list audit: %w", err)
		}
		r.TS, r.Forced = time.UnixMilli(ts).UTC(), forced != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	return out, nil
}
