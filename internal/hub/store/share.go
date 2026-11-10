package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The share rule of every Worker, as the Worker last declared it
// (0017_worker_share.sql). The Hub's event hub keeps it here so that what a
// restart or a reconnect does cannot lift a restriction: see
// workers.EventHub.

// LoadWorkerShares returns the stored share rule of every Worker, by Worker id.
func (s *Store) LoadWorkerShares(ctx context.Context) (map[string]ops.ShareDecl, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker_id, mode, names, revision FROM worker_share`)
	if err != nil {
		return nil, fmt.Errorf("store: load worker shares: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ops.ShareDecl{}
	for rows.Next() {
		var (
			id, mode, names string
			rev             int64
		)
		if err := rows.Scan(&id, &mode, &names, &rev); err != nil {
			return nil, fmt.Errorf("store: load worker shares: %w", err)
		}
		d := ops.ShareDecl{Rule: ops.ShareRule{Mode: mode}, Revision: uint64(rev)}
		if err := json.Unmarshal([]byte(names), &d.Rule.Names); err != nil {
			return nil, fmt.Errorf("store: load worker shares: the names of %s: %w", id, err)
		}
		if len(d.Rule.Names) == 0 {
			d.Rule.Names = nil
		}
		out[id] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load worker shares: %w", err)
	}
	return out, nil
}

// SaveWorkerShare stores the share rule of a Worker, replacing the one it had.
func (s *Store) SaveWorkerShare(ctx context.Context, workerID string, d ops.ShareDecl) error {
	if workerID == "" || d.Revision == 0 {
		return fmt.Errorf("store: save worker share: a Worker id and a revision are required")
	}
	names := d.Rule.Names
	if names == nil {
		names = []string{}
	}
	b, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("store: save worker share: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO worker_share (worker_id, mode, names, revision, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (worker_id) DO UPDATE SET mode = excluded.mode, names = excluded.names,
		     revision = excluded.revision, updated_at = excluded.updated_at`,
		workerID, d.Rule.Mode, string(b), int64(d.Revision), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: save worker share: %w", err)
	}
	return nil
}

// DeleteWorkerShare forgets the share rule of a Worker.
func (s *Store) DeleteWorkerShare(ctx context.Context, workerID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM worker_share WHERE worker_id = ?`, workerID); err != nil {
		return fmt.Errorf("store: delete worker share: %w", err)
	}
	return nil
}

// PendingShare is a restrictive share_set that the Hub dispatched and has not
// settled (0018_worker_share_pending.sql).
type PendingShare struct {
	Seq          uint64
	WorkerID     string
	Rule         ops.ShareRule
	BaseRevision uint64
}

// SavePendingShare stores a pending restriction. It is called before the change
// is sent to the Worker; if it fails nothing is sent.
func (s *Store) SavePendingShare(ctx context.Context, p PendingShare) error {
	if p.WorkerID == "" || p.Seq == 0 {
		return fmt.Errorf("store: save pending share: a Worker id and a sequence number are required")
	}
	names := p.Rule.Names
	if names == nil {
		names = []string{}
	}
	b, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("store: save pending share: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO worker_share_pending (seq, worker_id, mode, names, base_revision, created_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (seq) DO UPDATE SET worker_id = excluded.worker_id, mode = excluded.mode,
		     names = excluded.names, base_revision = excluded.base_revision`,
		int64(p.Seq), p.WorkerID, p.Rule.Mode, string(b), int64(p.BaseRevision), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: save pending share: %w", err)
	}
	return nil
}

// DeletePendingShares removes the pending restrictions of a Worker with a
// sequence number up to and including upTo (settled), or one exactly (seq).
func (s *Store) DeletePendingShares(ctx context.Context, workerID string, upTo uint64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM worker_share_pending WHERE worker_id = ? AND seq <= ?`, workerID, int64(upTo)); err != nil {
		return fmt.Errorf("store: delete pending shares: %w", err)
	}
	return nil
}

// DeletePendingShare removes one pending restriction.
func (s *Store) DeletePendingShare(ctx context.Context, workerID string, seq uint64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM worker_share_pending WHERE worker_id = ? AND seq = ?`, workerID, int64(seq)); err != nil {
		return fmt.Errorf("store: delete pending share: %w", err)
	}
	return nil
}

// LoadPendingShares returns every pending restriction, ordered by sequence
// number, and the highest sequence number ever written is not kept: the Hub
// derives new numbers from its clock and these rows.
func (s *Store) LoadPendingShares(ctx context.Context) ([]PendingShare, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, worker_id, mode, names, base_revision FROM worker_share_pending ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("store: load pending shares: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingShare
	for rows.Next() {
		var (
			p           PendingShare
			seq, baseRv int64
			names       string
		)
		if err := rows.Scan(&seq, &p.WorkerID, &p.Rule.Mode, &names, &baseRv); err != nil {
			return nil, fmt.Errorf("store: load pending shares: %w", err)
		}
		if err := json.Unmarshal([]byte(names), &p.Rule.Names); err != nil {
			return nil, fmt.Errorf("store: load pending shares: the names of %d: %w", seq, err)
		}
		if len(p.Rule.Names) == 0 {
			p.Rule.Names = nil
		}
		p.Seq, p.BaseRevision = uint64(seq), uint64(baseRv)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load pending shares: %w", err)
	}
	return out, nil
}
