package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The last instance of every session name of a Worker
// (0041_session_instance.sql): see workers.EventHub.FirstSeen.

// MaxSessionInstancesPerWorker bounds the rows kept for one Worker: the names
// seen least recently go first. It is the Hub's limit on the sessions of a
// Worker, twice.
const MaxSessionInstancesPerWorker = 8192

// LoadSessionInstances returns the last instance of every session name of the
// Worker that has one.
func (s *Store) LoadSessionInstances(ctx context.Context, workerID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_name, instance_id FROM session_instance WHERE worker_id = ?`, workerID)
	if err != nil {
		return nil, fmt.Errorf("store: load session instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, fmt.Errorf("store: load session instances: %w", err)
		}
		out[name] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load session instances: %w", err)
	}
	return out, nil
}

// SaveSessionInstances stores the instances of the given names of the Worker,
// replacing the ones they had, in one transaction. An empty instance is not
// stored. The oldest rows beyond [MaxSessionInstancesPerWorker] are dropped.
func (s *Store) SaveSessionInstances(ctx context.Context, workerID string, instances map[string]string) error {
	if workerID == "" {
		return fmt.Errorf("store: save session instances: a Worker id is required")
	}
	if len(instances) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for name, id := range instances {
			if name == "" || id == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO session_instance (worker_id, session_name, instance_id, seen_at) VALUES (?, ?, ?, ?)
				 ON CONFLICT (worker_id, session_name) DO UPDATE SET instance_id = excluded.instance_id, seen_at = excluded.seen_at`,
				workerID, name, id, now); err != nil {
				return fmt.Errorf("store: save session instance: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM session_instance WHERE worker_id = ?1 AND session_name NOT IN
			   (SELECT session_name FROM session_instance WHERE worker_id = ?1 ORDER BY seen_at DESC, session_name LIMIT ?2)`,
			workerID, MaxSessionInstancesPerWorker); err != nil {
			return fmt.Errorf("store: bound session instances: %w", err)
		}
		return nil
	})
}

// DeleteSessionInstances forgets the instances of a Worker (it was removed).
func (s *Store) DeleteSessionInstances(ctx context.Context, workerID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session_instance WHERE worker_id = ?`, workerID); err != nil {
		return fmt.Errorf("store: delete session instances: %w", err)
	}
	return nil
}
