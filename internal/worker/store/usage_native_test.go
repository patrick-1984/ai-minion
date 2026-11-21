package store

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestNativeUsageRetriesAndGenerationFence(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	now := time.Now()
	en := Enrollment{HubPub: pubKey(4), WorkerID: "worker", Owner: "test"}
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	id := ops.UsageID("native", "example")
	n := int64(20)
	row := ops.UsageSession{ID: id, NativeID: id, Name: "tty", AuthMode: "api", Source: "deepseek-response", ObservedAt: now.UnixMilli(), UsageCounters: ops.UsageCounters{InputTokens: &n, OutputTokens: &n}}
	r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Sessions: []ops.UsageSession{row}, Events: []ops.UsageEvent{{ID: ops.UsageID("event", "one"), NativeID: id, Kind: "turn_finished", Source: "deepseek-response", ObservedAt: now.UnixMilli()}}}
	if err := s.SaveNativeUsage(ctx, en, r, now); err == nil {
		t.Fatal("accepted unregistered telemetry")
	}
	if err := s.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.SaveNativeUsage(ctx, en, r, now); err != nil {
			t.Fatal(err)
		}
	}
	r.Events[0].ID = ops.UsageID("event", "two")
	if err := s.SaveNativeUsage(ctx, en, r, now); err != nil {
		t.Fatal(err)
	}
	hostID := ops.UsageID("host", "synthetic")
	started := now.Add(-time.Hour).UnixMilli()
	report := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Sessions: []ops.UsageSession{{ID: hostID, Name: "tty", StartedAt: &started}}}
	if err := s.NativeUsage(ctx, &report, 0); err != nil {
		t.Fatal(err)
	}
	if got := *report.Sessions[0].InputTokens; got != 40 {
		t.Fatalf("cumulative %d, want 40", got)
	}
	if len(report.Events) != 2 || report.Events[1].SessionID != hostID {
		t.Fatalf("events %+v", report.Events)
	}
	report.Events = nil
	if err := s.NativeUsage(ctx, &report, report.ObservedAt); err != nil || len(report.Events) != 0 {
		t.Fatal("replayed acknowledged event")
	}
	changed := en
	changed.Owner = "next"
	if err := s.SaveEnrollment(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptUsageRegistration(ctx, changed.HubPub, changed.WorkerID, changed.Owner); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveNativeUsage(ctx, en, r, now); err == nil {
		t.Fatal("accepted old owner after new registration")
	}
}
