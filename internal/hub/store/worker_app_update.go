package store

// How the Hub updates a Worker (v0.24.0), table worker_app_update. A Worker
// without a row is manual: only an administrator's Update button updates it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Stored policies of worker_app_update; manual is the absence of a row.
const (
	WorkerAppUpdateAuto = "auto"
	WorkerAppUpdateOff  = "off"
)

// WorkerAppUpdate is the stored update policy of one Worker.
type WorkerAppUpdate struct {
	WorkerID string
	// Policy is auto or off.
	Policy string
	// UpdatedBy is the user id of the administrator who set it.
	UpdatedBy string
	UpdatedAt time.Time
}

// ListWorkerAppUpdates returns the policy of every Worker that has one, by
// Worker id.
func (s *Store) ListWorkerAppUpdates(ctx context.Context) (map[string]WorkerAppUpdate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker_id, policy, updated_by, updated_at FROM worker_app_update`)
	if err != nil {
		return nil, fmt.Errorf("store: list worker app updates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]WorkerAppUpdate{}
	for rows.Next() {
		var p WorkerAppUpdate
		var at int64
		if err := rows.Scan(&p.WorkerID, &p.Policy, &p.UpdatedBy, &at); err != nil {
			return nil, fmt.Errorf("store: list worker app updates: %w", err)
		}
		p.UpdatedAt = time.UnixMilli(at)
		out[p.WorkerID] = p
	}
	return out, rows.Err()
}

// WorkerAppUpdatePolicy returns the policy of one Worker: auto, off, or ""
// for manual. It does not check that the Worker exists.
func (s *Store) WorkerAppUpdatePolicy(ctx context.Context, workerID string) (string, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT policy FROM worker_app_update WHERE worker_id = ?`, workerID).Scan(&p)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("store: read worker app update: %w", err)
	}
	return p, nil
}

// SetWorkerAppUpdate stores the policy of a Worker (auto or off), or removes
// it for "" or "manual". by is the user id of the administrator. It returns
// [ErrWorkerNotFound] for an unknown or removed Worker.
func (s *Store) SetWorkerAppUpdate(ctx context.Context, workerID, policy, by string) error {
	switch policy {
	case "", "manual", WorkerAppUpdateAuto, WorkerAppUpdateOff:
	default:
		return fmt.Errorf("store: unknown worker app update policy %q", policy)
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM workers WHERE id = ?`, workerID).Scan(&one)
		if err := rowErr("read worker", err); err != nil {
			return err
		}
		if policy == "" || policy == "manual" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM worker_app_update WHERE worker_id = ?`, workerID); err != nil {
				return fmt.Errorf("store: clear worker app update: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO worker_app_update (worker_id, policy, updated_by, updated_at) VALUES (?1, ?2, ?3, ?4)
			 ON CONFLICT (worker_id) DO UPDATE SET policy = ?2, updated_by = ?3, updated_at = ?4`,
			workerID, policy, by, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("store: set worker app update: %w", err)
		}
		return nil
	})
}

// WorkerAppUpdatePolicies returns the policy (auto or off) of every Worker
// that has one, by Worker id.
func (s *Store) WorkerAppUpdatePolicies(ctx context.Context) (map[string]string, error) {
	all, err := s.ListWorkerAppUpdates(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for id, p := range all {
		out[id] = p.Policy
	}
	return out, nil
}
