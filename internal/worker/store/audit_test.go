package store

import (
	"context"
	"testing"
	"time"
)

func TestAuditAppendAndList(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))

	if rows, err := s.ListAudit(ctx, 10); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty list = %v, %v; want empty non-nil", rows, err)
	}
	if err := s.AppendAudit(ctx, AuditRecord{Op: "send_text", Target: ":work", Outcome: "delivered", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if err := s.AppendAudit(ctx, AuditRecord{Op: "send_keys", Target: ":work:0.1", Outcome: "failed_not_sent", Reason: "busy", Forced: true, RequestID: "r2"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAudit(ctx, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if rows[0].RequestID != "r2" || rows[0].Reason != "busy" || !rows[0].Forced || rows[0].Op != "send_keys" {
		t.Fatalf("newest row = %+v", rows[0])
	}
	if rows[1].RequestID != "r1" || rows[1].Forced || !rows[1].TS.Equal(clk.Now().Add(-time.Second)) {
		t.Fatalf("oldest row = %+v", rows[1])
	}
	if one, _ := s.ListAudit(ctx, 1); len(one) != 1 || one[0].RequestID != "r2" {
		t.Fatalf("limit 1 = %+v", one)
	}
}

func TestAuditRejectsIncompleteRecord(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.AppendAudit(context.Background(), AuditRecord{Op: "send_text"}); err == nil {
		t.Fatal("record without outcome accepted")
	}
}

func TestAuditIsTrimmedToCap(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	// Seed the table near the cap directly, then append through the API.
	if _, err := s.DB().Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO local_audit (ts, op, target, outcome, reason, forced, request_id)
		SELECT 0, 'x', 't', 'delivered', '', 0, 'r' FROM n`, AuditCap); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit(ctx, AuditRecord{Op: "send_text", Outcome: "delivered", RequestID: "new"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM local_audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != AuditCap {
		t.Fatalf("rows = %d, want %d", n, AuditCap)
	}
	rows, _ := s.ListAudit(ctx, 1)
	if len(rows) != 1 || rows[0].RequestID != "new" {
		t.Fatalf("newest = %+v", rows)
	}
}
