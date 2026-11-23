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

func TestNativeUsageObservedAPIAccessBeatsTheProfileSubscription(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	now := time.Now()
	en := Enrollment{HubPub: pubKey(5), WorkerID: "worker", Owner: "test"}
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	subID := ops.UsageID("subscription", "profile")
	id := ops.UsageID("native", "api-session")
	row := ops.UsageSession{ID: id, NativeID: id, Name: "tty", Provider: "Anthropic", AccountID: subID, AuthMode: "subscription", Source: "claude-statusline", ObservedAt: now.UnixMilli()}
	r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Sessions: []ops.UsageSession{row},
		Accounts: []ops.UsageAccount{{ID: subID, Provider: "Anthropic", Label: "me@example.org", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: now.UnixMilli(), TimeSource: "worker_measured"}}}
	if err := s.SaveNativeUsage(ctx, en, r, now); err != nil {
		t.Fatal(err)
	}
	apiID := ops.UsageID("session-access", "Anthropic", "gw.example")
	started := now.Add(-time.Hour).UnixMilli()
	report := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(),
		Sessions: []ops.UsageSession{{ID: ops.UsageID("host", "x"), Name: "tty", StartedAt: &started, Provider: "Anthropic", AuthMode: "api", Endpoint: "gw.example", AccountID: apiID}},
		Accounts: []ops.UsageAccount{{ID: apiID, Provider: "Anthropic", AuthMode: "api", Label: "API keys at gw.example (not told apart)"}}}
	if err := s.NativeUsage(ctx, &report, 0); err != nil {
		t.Fatal(err)
	}
	got := report.Sessions[0]
	if got.AuthMode != "api" || got.Endpoint != "gw.example" || got.AccountID != apiID {
		t.Fatalf("session relabelled as the subscription: %+v", got)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].ID != apiID {
		t.Fatalf("subscription account attributed to the API session: %+v", report.Accounts)
	}
}
