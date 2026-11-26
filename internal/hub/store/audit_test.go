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

// A brokered write that overlapping grants authorized names every grant in
// one comma-separated grant_id: the history of each of them finds it, and an
// id that is only a part of a stored id finds nothing.
func TestListAuditByGrantFindsEachOverlappingGrant(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	wildcard, named := "0190a000-0000-7000-8000-000000000001", "0190a000-0000-7000-8000-000000000002"
	for i, g := range []string{wildcard + "," + named, named, wildcard + "x", ""} {
		if err := s.InsertAudit(ctx, nil, AuditRow{ID: NewID(), TS: int64(100 + i), Actor: "worker:w", Action: "send_text", Outcome: "success", GrantID: g}); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string][]int64{wildcard: {100}, named: {101, 100}} {
		rows, err := s.ListAudit(ctx, AuditQuery{GrantID: id, Limit: 10})
		if err != nil || len(rows) != len(want) {
			t.Fatalf("history of %s = %+v, %v", id, rows, err)
		}
		for i, r := range rows {
			if r.TS != want[i] {
				t.Fatalf("history of %s = %+v", id, rows)
			}
		}
	}
	for _, id := range []string{"0190a000-0000-7000-8000-00000000000", wildcard[1:], ","} {
		if rows, err := s.ListAudit(ctx, AuditQuery{GrantID: id, Limit: 10}); err != nil || len(rows) != 0 {
			t.Fatalf("a part of an id %q matched %+v, %v", id, rows, err)
		}
	}
}
