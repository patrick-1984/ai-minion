package store

import (
	"context"
	"fmt"
)

// ListWorkers returns every enrolled Worker, ordered by name, then id. It
// reads the table on every call (no cache): routing and workers_list depend on
// current ownership (AD-11).
func (s *Store) ListWorkers(ctx context.Context) ([]WorkerRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+workerColumns+` FROM workers ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list workers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkerRow
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list workers: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list workers: %w", err)
	}
	return out, nil
}
