package store

import (
	"context"
	"strings"
	"testing"
)

func TestAuditCursorQueryUsesTheIndexRange(t *testing.T) {
	s, err := openCached(t, t.TempDir())
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

func TestAuditRoundTripsSourceGrantAndDetail(t *testing.T) {
	s, err := openCached(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	old := AuditRow{ID: "id-1", TS: 1, Actor: "a", Action: "x", Outcome: "success"}
	full := AuditRow{ID: "id-2", TS: 2, Actor: "worker:w1", Action: "send_text", Target: "w2:build", Outcome: "denied",
		Source: "w1:boss", GrantID: "g-1", Detail: "proof=name"}
	for _, r := range []AuditRow{old, full} {
		if err := s.InsertAudit(ctx, nil, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListAudit(ctx, AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != full || rows[1] != old {
		t.Fatalf("rows = %+v", rows)
	}
}
