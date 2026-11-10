package store

import (
	"context"
	"strings"
	"testing"
)

func TestAuditCursorQueryUsesTheIndexRange(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, q := range []AuditQuery{
		{After: &AuditPos{TS: 1000, ID: "z"}, Limit: 10},
		{Actor: "a", After: &AuditPos{TS: 1000, ID: "z"}, Limit: 10},
		{Action: "x", After: &AuditPos{TS: 1000, ID: "z"}, Limit: 10},
	} {
		query, args := auditSelect(q)
		rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		joined := strings.Join(plan, "; ")
		if !strings.Contains(joined, "SEARCH") || strings.Contains(joined, "SCAN") || !strings.Contains(joined, "<") {
			t.Errorf("query %+v plan = %q, want an indexed range SEARCH on (ts, id)", q, joined)
		}
	}
}
