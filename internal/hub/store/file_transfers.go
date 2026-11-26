package store

// File transfer records (files.v1, migration 0042_file_transfer.sql): the
// durable record of every transfer attempt, written before any byte moves,
// its terminal state and entries, the owner-scoped history with retention,
// the Hub's per-Worker override and the file flags of master acceptances.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Limits of the stored history.
const (
	// MaxStoredFileEntries is the number of entries kept per transfer.
	MaxStoredFileEntries = 1000
	// DefaultFileTransferRetention is file_transfer_retention_days' default.
	DefaultFileTransferRetention = 90 * 24 * time.Hour
)

var (
	// ErrFileTransferExists is returned when a record with that id exists.
	ErrFileTransferExists = errors.New("store: the file transfer exists")
	// ErrFileTransferState is returned for a state change that the record's
	// current state does not allow (for example prepared after delivered).
	ErrFileTransferState = errors.New("store: the file transfer is not in a state that allows this change")
	// ErrFileTransferInvalid is returned for a record that breaks a rule of
	// the table (unknown direction or state, a missing target).
	ErrFileTransferInvalid = errors.New("store: invalid file transfer record")
)

// FileTransferRow is one row of file_transfers. Times are unix milliseconds.
type FileTransferRow struct {
	ID              string
	Direction       string // ops.FileDirection*
	State           string // ops.FileState*
	Reason          string // a proto code, "" when delivered
	Principal       string // display form: user:<id>, key:<id>, worker:<id>
	PrincipalUserID string // the user whose history it is (key owner, session user)
	ActingAs        string
	SourceWorkerID  string // "" for a REST or browser caller
	SourceSession   string
	SourceOwnerID   string
	TargetWorkerID  string
	TargetSession   string
	TargetOwnerID   string
	GrantIDs        []string
	AcceptanceID    string
	Policy          string // policy snapshot in the audit detail grammar
	CallerRoot      string // display roots, never absolute under home
	TargetRoot      string
	Path            string // relative to the target root
	Kind            string // "", ops.FileKindFile or ops.FileKindDir
	Files           int64
	Bytes           int64
	WireBytes       int64
	ManifestDigest  string
	Protected       string
	EntriesOmitted  int64
	// Receiver is the identity of the receiving leg (for example
	// "worker:<id>"), ReceiptHash the lower-case hex SHA-256 of its leg
	// token: a late receipt is checked against them after a Hub restart. The
	// token itself is never stored.
	Receiver    string
	ReceiptHash string
	CreatedAt   int64
	UpdatedAt   int64
	// DeadlineAt ends a record that is not terminal: reconciliation then
	// turns it unknown. Nil for a terminal record.
	DeadlineAt *int64
	EndedAt    *int64
}

// fileStateOpen reports whether a record in state s is not terminal yet.
func fileStateOpen(s string) bool { return s == ops.FileStateAdmitted || s == ops.FileStatePrepared }

// fileStateFinal reports whether a record in state s never changes again.
// unknown is terminal but can still move to its real outcome.
func fileStateFinal(s string) bool { return !fileStateOpen(s) && s != ops.FileStateUnknown }

// fileOutcomeState reports whether s is a state a transfer can end in.
func fileOutcomeState(s string) bool {
	switch s {
	case ops.FileStateDelivered, ops.FileStatePartial, ops.FileStateFailedNotSent, ops.FileStateServed, ops.FileStateUnknown:
		return true
	}
	return false
}

const fileTransferCols = `id, direction, state, reason, principal, principal_user_id, acting_as,
	source_worker_id, source_session, source_owner_id, target_worker_id, target_session, target_owner_id,
	grant_ids, acceptance_id, policy, caller_root, target_root, path, kind, files, bytes, wire_bytes,
	manifest_digest, protected, entries_omitted, receiver, receipt_hash, created_at, updated_at, deadline_at, ended_at`

func scanFileTransfer(sc rowScanner) (FileTransferRow, error) {
	var r FileTransferRow
	var grants string
	var deadline, ended sql.NullInt64
	err := sc.Scan(&r.ID, &r.Direction, &r.State, &r.Reason, &r.Principal, &r.PrincipalUserID, &r.ActingAs,
		&r.SourceWorkerID, &r.SourceSession, &r.SourceOwnerID, &r.TargetWorkerID, &r.TargetSession, &r.TargetOwnerID,
		&grants, &r.AcceptanceID, &r.Policy, &r.CallerRoot, &r.TargetRoot, &r.Path, &r.Kind, &r.Files, &r.Bytes,
		&r.WireBytes, &r.ManifestDigest, &r.Protected, &r.EntriesOmitted, &r.Receiver, &r.ReceiptHash,
		&r.CreatedAt, &r.UpdatedAt, &deadline, &ended)
	if err != nil {
		return r, err
	}
	if grants != "" {
		// An unreadable list names no grant.
		_ = json.Unmarshal([]byte(grants), &r.GrantIDs)
	}
	if deadline.Valid {
		r.DeadlineAt = &deadline.Int64
	}
	if ended.Valid {
		r.EndedAt = &ended.Int64
	}
	return r, nil
}

func grantIDsJSON(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

// InsertFileTransfer stores the admission record of a transfer before any
// byte moves (BRD 5.9): state admitted (with a deadline, after which
// reconciliation turns it unknown) or denied (terminal, no digest). A failure
// is returned and the caller refuses the transfer (internal); an existing id
// is [ErrFileTransferExists]. A zero CreatedAt is now; UpdatedAt follows it.
func (s *Store) InsertFileTransfer(ctx context.Context, r FileTransferRow) error {
	if r.ID == "" || r.TargetWorkerID == "" || (r.Direction != ops.FileDirectionPull && r.Direction != ops.FileDirectionPush) {
		return ErrFileTransferInvalid
	}
	switch r.State {
	case ops.FileStateAdmitted:
		if r.DeadlineAt == nil {
			return fmt.Errorf("%w: an admitted transfer needs a deadline", ErrFileTransferInvalid)
		}
		r.EndedAt = nil
	case ops.FileStateDenied:
		r.DeadlineAt, r.ManifestDigest = nil, ""
	default:
		return fmt.Errorf("%w: a transfer starts admitted or denied", ErrFileTransferInvalid)
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = NowMS()
	}
	r.UpdatedAt = r.CreatedAt
	if r.State == ops.FileStateDenied {
		ended := r.CreatedAt
		r.EndedAt = &ended
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO file_transfers (`+fileTransferCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Direction, r.State, r.Reason, r.Principal, r.PrincipalUserID, r.ActingAs,
		r.SourceWorkerID, r.SourceSession, r.SourceOwnerID, r.TargetWorkerID, r.TargetSession, r.TargetOwnerID,
		grantIDsJSON(r.GrantIDs), r.AcceptanceID, r.Policy, r.CallerRoot, r.TargetRoot, r.Path, r.Kind,
		r.Files, r.Bytes, r.WireBytes, r.ManifestDigest, r.Protected, r.EntriesOmitted, r.Receiver, r.ReceiptHash,
		r.CreatedAt, r.UpdatedAt, nullInt64(r.DeadlineAt), nullInt64(r.EndedAt))
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "UNIQUE constraint") || strings.Contains(msg, "PRIMARY KEY"):
			return ErrFileTransferExists
		case strings.Contains(msg, "CHECK constraint"):
			return fmt.Errorf("%w: %v", ErrFileTransferInvalid, err)
		}
		return fmt.Errorf("store: insert file transfer: %w", err)
	}
	return nil
}

// FileTransfer returns one record, or [ErrNotFound].
func (s *Store) FileTransfer(ctx context.Context, id string) (FileTransferRow, error) {
	r, err := scanFileTransfer(s.db.QueryRowContext(ctx, `SELECT `+fileTransferCols+` FROM file_transfers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("store: read file transfer: %w", err)
	}
	return r, nil
}

// FilePrepared is what the target's admission answer adds to an admitted
// record.
type FilePrepared struct {
	TargetRoot string
	Kind       string
	// Bytes is the declared size (a single file of a pull, or what a push
	// declares); 0 when unknown.
	Bytes int64
	// DeadlineAt replaces the deadline (nil keeps it).
	DeadlineAt *int64
}

// MarkFileTransferPrepared moves an admitted record to prepared. Any other
// current state is [ErrFileTransferState]; a missing record [ErrNotFound].
func (s *Store) MarkFileTransferPrepared(ctx context.Context, id string, p FilePrepared, now int64) error {
	if now == 0 {
		now = NowMS()
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM file_transfers WHERE id = ?`, id).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: read file transfer: %w", err)
		}
		if state != ops.FileStateAdmitted {
			return ErrFileTransferState
		}
		_, err := tx.ExecContext(ctx, `UPDATE file_transfers SET state = 'prepared', target_root = ?, kind = ?, bytes = ?,
			deadline_at = COALESCE(?, deadline_at), updated_at = ? WHERE id = ?`,
			p.TargetRoot, p.Kind, p.Bytes, nullInt64(p.DeadlineAt), now, id)
		if err != nil {
			return fmt.Errorf("store: prepare file transfer: %w", err)
		}
		return nil
	})
}

// FileOutcome is the terminal statement of a transfer: a receipt, a web
// download's bytes served, a failure or reconciliation.
type FileOutcome struct {
	// State is delivered, partial, failed_not_sent, served or unknown.
	State          string
	Reason         string
	Files          int64
	Bytes          int64
	WireBytes      int64
	ManifestDigest string
	Protected      string
	// Entries are stored up to MaxStoredFileEntries; the rest is counted in
	// EntriesOmitted together with the caller's own omitted count.
	Entries        []ops.FileTransferEntry
	EntriesOmitted int64
}

// FinishFileTransfer ends a record: from admitted, prepared or unknown to
// out.State, with totals and entries, in one transaction. A record already in
// a final state (delivered, partial, failed_not_sent, served, denied) is not
// changed: changed is false and the stored row is returned, so a repeated
// receipt (outbox resend) is acknowledged with the state it reached. unknown
// can move to its real outcome once; out.State unknown never overwrites an
// unknown record's totals. A missing record is [ErrNotFound].
func (s *Store) FinishFileTransfer(ctx context.Context, id string, out FileOutcome, now int64) (row FileTransferRow, changed bool, err error) {
	if !fileOutcomeState(out.State) {
		return FileTransferRow{}, false, fmt.Errorf("%w: %q is not an outcome", ErrFileTransferInvalid, out.State)
	}
	if now == 0 {
		now = NowMS()
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := scanFileTransfer(tx.QueryRowContext(ctx, `SELECT `+fileTransferCols+` FROM file_transfers WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read file transfer: %w", err)
		}
		if fileStateFinal(cur.State) || cur.State == ops.FileStateUnknown && out.State == ops.FileStateUnknown {
			row = cur
			return nil
		}
		entries := out.Entries
		omitted := out.EntriesOmitted
		if len(entries) > MaxStoredFileEntries {
			omitted += int64(len(entries) - MaxStoredFileEntries)
			entries = entries[:MaxStoredFileEntries]
		}
		ended := now
		if cur.EndedAt != nil && cur.State == ops.FileStateUnknown {
			ended = *cur.EndedAt
		}
		if _, err := tx.ExecContext(ctx, `UPDATE file_transfers SET state = ?, reason = ?, files = ?, bytes = ?, wire_bytes = ?,
			manifest_digest = ?, protected = ?, entries_omitted = ?, deadline_at = NULL, ended_at = ?, updated_at = ? WHERE id = ?`,
			out.State, out.Reason, out.Files, out.Bytes, out.WireBytes, out.ManifestDigest, out.Protected, omitted,
			ended, now, id); err != nil {
			if strings.Contains(err.Error(), "CHECK constraint") {
				return fmt.Errorf("%w: %v", ErrFileTransferInvalid, err)
			}
			return fmt.Errorf("store: finish file transfer: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_transfer_entries WHERE transfer_id = ?`, id); err != nil {
			return fmt.Errorf("store: finish file transfer: %w", err)
		}
		for i, e := range entries {
			if _, err := tx.ExecContext(ctx, `INSERT INTO file_transfer_entries (transfer_id, seq, path, size, sha256, skipped_reason)
				VALUES (?, ?, ?, ?, ?, ?)`, id, i, e.Path, e.Size, e.SHA256, e.SkippedReason); err != nil {
				return fmt.Errorf("store: store file transfer entries: %w", err)
			}
		}
		changed = true
		row, err = scanFileTransfer(tx.QueryRowContext(ctx, `SELECT `+fileTransferCols+` FROM file_transfers WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return FileTransferRow{}, false, err
	}
	return row, changed, nil
}

// FileTransferEntries returns the stored entries of a transfer in order.
func (s *Store) FileTransferEntries(ctx context.Context, id string) ([]ops.FileTransferEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, size, sha256, skipped_reason FROM file_transfer_entries
		WHERE transfer_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("store: read file transfer entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ops.FileTransferEntry
	for rows.Next() {
		var e ops.FileTransferEntry
		if err := rows.Scan(&e.Path, &e.Size, &e.SHA256, &e.SkippedReason); err != nil {
			return nil, fmt.Errorf("store: read file transfer entries: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReconcileFileTransfers turns every record that is still admitted or
// prepared and whose deadline is before now into unknown (BRD 5.9: at Hub
// start and every 10 minutes). It returns the ids it changed.
func (s *Store) ReconcileFileTransfers(ctx context.Context, now int64) ([]string, error) {
	var ids []string
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM file_transfers WHERE state IN ('admitted', 'prepared')
			AND deadline_at IS NOT NULL AND deadline_at < ? ORDER BY id`, now)
		if err != nil {
			return fmt.Errorf("store: reconcile file transfers: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE file_transfers SET state = 'unknown', deadline_at = NULL, ended_at = ?,
				updated_at = ? WHERE id = ? AND state IN ('admitted', 'prepared')`, now, now, id); err != nil {
				return fmt.Errorf("store: reconcile file transfers: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// PurgeFileTransfers deletes the records (and their entries) created before
// before, the retention limit, and returns how many it deleted.
func (s *Store) PurgeFileTransfers(ctx context.Context, before int64) (int64, error) {
	var n int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_transfer_entries WHERE transfer_id IN
			(SELECT id FROM file_transfers WHERE created_at < ?)`, before); err != nil {
			return fmt.Errorf("store: purge file transfers: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM file_transfers WHERE created_at < ?`, before)
		if err != nil {
			return fmt.Errorf("store: purge file transfers: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// FileTransferFilter selects the history of file_transfers_list.
type FileTransferFilter struct {
	// ViewerID is the user who reads; Admin lists every record. Without
	// Admin a record is listed when ViewerID owned its source or target
	// Worker at admission or is its principal's user.
	ViewerID string
	Admin    bool
	// WorkerID keeps the records that touch that Worker (either end).
	WorkerID string
	// TransferID keeps one record.
	TransferID string
	// Cursor is the next cursor of the previous page ("" for the first).
	Cursor string
	// Limit caps the page (0: ops.FileTransfersDefaultLimit).
	Limit int
}

// ErrFileCursor is returned for a cursor that ListFileTransfers did not make.
var ErrFileCursor = errors.New("store: invalid file transfer cursor")

func fileCursor(createdAt int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(createdAt, 10) + ":" + id))
}

func parseFileCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", ErrFileCursor
	}
	at, id, ok := strings.Cut(string(b), ":")
	n, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || id == "" {
		return 0, "", ErrFileCursor
	}
	return n, id, nil
}

// ListFileTransfers returns one page of the owner-scoped history, newest
// first (created_at, then id, descending), and the cursor of the next page
// ("" on the last). A viewer that is neither an administrator nor named gets
// an empty page; an empty ViewerID without Admin lists nothing.
func (s *Store) ListFileTransfers(ctx context.Context, f FileTransferFilter) ([]FileTransferRow, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = ops.FileTransfersDefaultLimit
	}
	if limit > ops.FileTransfersMaxLimit {
		limit = ops.FileTransfersMaxLimit
	}
	if !f.Admin && f.ViewerID == "" {
		return nil, "", nil
	}
	var where []string
	var args []any
	if !f.Admin {
		where = append(where, `(source_owner_id = ? OR target_owner_id = ? OR principal_user_id = ?)`)
		args = append(args, f.ViewerID, f.ViewerID, f.ViewerID)
	}
	if f.WorkerID != "" {
		where = append(where, `(source_worker_id = ? OR target_worker_id = ?)`)
		args = append(args, f.WorkerID, f.WorkerID)
	}
	if f.TransferID != "" {
		where = append(where, `id = ?`)
		args = append(args, f.TransferID)
	}
	if f.Cursor != "" {
		at, id, err := parseFileCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, `(created_at < ? OR created_at = ? AND id < ?)`)
		args = append(args, at, at, id)
	}
	q := `SELECT ` + fileTransferCols + ` FROM file_transfers`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list file transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FileTransferRow
	for rows.Next() {
		r, err := scanFileTransfer(rows)
		if err != nil {
			return nil, "", fmt.Errorf("store: list file transfers: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list file transfers: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = fileCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// Record converts the row to its history form, with entries (stored order).
// The receiver identity and the receipt hash are left out.
func (r FileTransferRow) Record(entries []ops.FileTransferEntry) ops.FileTransferRecord {
	rec := ops.FileTransferRecord{
		TransferID: r.ID, Direction: r.Direction, State: r.State, Reason: r.Reason,
		Principal: r.Principal, ActingAs: r.ActingAs,
		SourceWorkerID: r.SourceWorkerID, SourceSession: r.SourceSession,
		TargetWorkerID: r.TargetWorkerID, TargetSession: r.TargetSession,
		AcceptanceID: r.AcceptanceID, CallerRoot: r.CallerRoot, TargetRoot: r.TargetRoot,
		Path: r.Path, Kind: r.Kind, Files: r.Files, Bytes: r.Bytes, WireBytes: r.WireBytes,
		ManifestDigest: r.ManifestDigest, Protected: r.Protected,
		Entries: entries, EntriesOmitted: r.EntriesOmitted,
		CreatedAt: ops.Timestamp(FromUnixMS(r.CreatedAt)),
	}
	if len(r.GrantIDs) == 1 {
		rec.GrantID = r.GrantIDs[0]
	} else if len(r.GrantIDs) > 1 {
		// The record names one grant; the first one that authorized it.
		rec.GrantID = r.GrantIDs[0]
	}
	if r.EndedAt != nil {
		t := ops.Timestamp(FromUnixMS(*r.EndedAt))
		rec.EndedAt = &t
	}
	return rec
}

// WorkerFileMode returns the Hub's per-Worker file transfer override of a
// Worker (ops.FileMode*), or [ErrWorkerNotFound].
func (s *Store) WorkerFileMode(ctx context.Context, workerID string) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT file_transfer FROM workers WHERE id = ?`, workerID).Scan(&mode)
	if err := rowErr("read worker file transfer", err); err != nil {
		return "", err
	}
	return mode, nil
}

// SetWorkerFileMode sets the Hub's per-Worker override and reports whether
// it changed. An unknown mode is [ErrFileTransferInvalid]; an unknown Worker
// [ErrWorkerNotFound].
func (s *Store) SetWorkerFileMode(ctx context.Context, workerID, mode string) (changed bool, err error) {
	if !ops.ValidWorkerFileMode(mode) {
		return false, ErrFileTransferInvalid
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var cur string
		if err := rowErr("read worker file transfer", tx.QueryRowContext(ctx, `SELECT file_transfer FROM workers WHERE id = ?`, workerID).Scan(&cur)); err != nil {
			return err
		}
		if cur == mode {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workers SET file_transfer = ? WHERE id = ?`, mode, workerID); err != nil {
			return fmt.Errorf("store: set worker file transfer: %w", err)
		}
		changed = true
		return nil
	})
	return changed, err
}

// AcceptanceID is the identity of the master acceptance row of target for
// source (its primary key), as decisions and transfer records name it.
func AcceptanceID(target, source string) string { return target + "/" + source }

// FileAcceptances returns the acceptance rows that accept control from
// source and carry a file flag, ordered by target. The flags count only
// through these rows: a target that accepts all Workers (accept_all) without
// a row has none.
func (s *Store) FileAcceptances(ctx context.Context, source string) ([]AcceptRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target_worker_id, source_worker_id, sessions_json, created_by, created_at,
		files_pull, files_push FROM control_accept WHERE source_worker_id = ? AND (files_pull = 1 OR files_push = 1)
		ORDER BY target_worker_id`, source)
	if err != nil {
		return nil, fmt.Errorf("store: list file acceptances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAccepts(rows)
}
