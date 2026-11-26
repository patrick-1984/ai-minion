package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
)

// Rows that SQL inserts by itself are forwarded to the observer exactly once,
// oldest first, also when many commits forward at the same time and across
// more than one batch; rows written through InsertAudit are not forwarded
// again.
func TestForwardSQLAuditExactlyOnce(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	var (
		mu   sync.Mutex
		seen = map[string]int{}
		ord  []string
	)
	s.SetAuditObserver(func(_ context.Context, r AuditRow) {
		mu.Lock()
		defer mu.Unlock()
		seen[r.ID]++
		ord = append(ord, r.ID)
	})
	if err := s.InsertAudit(ctx, nil, AuditRow{ID: NewID(), TS: 1, Actor: "system", Action: "go.row", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	const n = auditExportBatch*2 + 7
	var want []string
	if err := Tx(ctx, s.db, func(tx *sql.Tx) error { // the package Tx: no forwarding yet
		for i := range n {
			id := fmt.Sprintf("sql-%04d", i)
			want = append(want, id)
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events (id, ts, actor, action, outcome) VALUES (?, ?, 'system', 'sql.row', 'success')`, id, 1000-i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := s.Tx(ctx, func(*sql.Tx) error { return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(ord) != n+1 {
		t.Fatalf("observer saw %d rows, want %d", len(ord), n+1)
	}
	for id, k := range seen {
		if k != 1 {
			t.Fatalf("row %s seen %d times", id, k)
		}
	}
	for i, id := range want {
		if ord[i+1] != id {
			t.Fatalf("row %d is %s, want %s (insertion order)", i, ord[i+1], id)
		}
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE exported = 0`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("unexported rows = %d, %v", left, err)
	}
}
