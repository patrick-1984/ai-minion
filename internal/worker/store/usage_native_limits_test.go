package store

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func limitsStore(t *testing.T) (*Store, Enrollment) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	en := Enrollment{HubPub: pubKey(7), WorkerID: "worker", Owner: "test"}
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	return s, en
}

// An idle Claude session re-runs its status line every few seconds with the
// rate limits of its last API response. Those re-sends must neither look
// newer than an active session's limits nor replace them.
func TestNativeUsageIdleResendDoesNotFlipAccountLimits(t *testing.T) {
	ctx := context.Background()
	s, en := limitsStore(t)
	account := ops.UsageID("acct", "formula")
	five, week := (5 * time.Hour).Milliseconds(), (7 * 24 * time.Hour).Milliseconds()
	start := time.Now().Add(-10 * time.Minute)
	weekReset := start.Add(5 * 24 * time.Hour).UnixMilli()
	fiveReset := start.Add(3 * time.Hour).UnixMilli()
	pct := func(v float64) *float64 { return &v }
	save := func(name, native string, at time.Time, api int64, windows ...ops.UsageWindow) {
		t.Helper()
		id := ops.UsageID("native", native)
		row := ops.UsageSession{ID: id, NativeID: id, Name: name, Provider: "Anthropic", AccountID: account, AuthMode: "subscription", Source: "claude-statusline",
			ObservedAt: at.UnixMilli(), UsageCounters: ops.UsageCounters{APIDurationMS: &api}}
		a := ops.UsageAccount{ID: account, Provider: "Anthropic", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: at.UnixMilli(), Windows: windows}
		if err := s.SaveNativeUsage(ctx, en, ops.UsageReport{Version: 1, ObservedAt: at.UnixMilli(), Sessions: []ops.UsageSession{row}, Accounts: []ops.UsageAccount{a}}, at); err != nil {
			t.Fatal(err)
		}
	}
	weekly := func(v float64) ops.UsageWindow {
		return ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &week, UsedPercent: pct(v), ResetsAt: &weekReset}
	}
	short := ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &five, UsedPercent: pct(3), ResetsAt: &fiveReset}
	// A (idle) was last active at start with 4% and no five-hour window.
	save("wb-5", "idle", start, 100, weekly(4))
	// B (active) reports 20% and a five-hour window a minute later.
	activeAt := start.Add(time.Minute)
	save("wb-1", "active", activeAt, 500, short, weekly(20))
	// A re-sends its unchanged limits every 5 s, long after B's report.
	var last time.Time
	for i := range 6 {
		last = activeAt.Add(time.Duration(i+1) * 5 * time.Second)
		save("wb-5", "idle", last, 100, weekly(4))
	}
	started := start.Add(-time.Hour).UnixMilli()
	report := ops.UsageReport{Version: 1, ObservedAt: last.UnixMilli(), Sessions: []ops.UsageSession{
		{ID: ops.UsageID("host", "wb-1"), Name: "wb-1", StartedAt: &started},
		{ID: ops.UsageID("host", "wb-5"), Name: "wb-5", StartedAt: &started},
	}}
	if err := s.NativeUsage(ctx, &report, 0); err != nil {
		t.Fatal(err)
	}
	if len(report.Accounts) != 2 {
		t.Fatalf("want one account row per session: %+v", report.Accounts)
	}
	for _, a := range report.Accounts {
		if a.ObservedAt != activeAt.UnixMilli() {
			t.Fatalf("limits time %d, want the active report %d (idle re-sends must not advance it)", a.ObservedAt, activeAt.UnixMilli())
		}
		if len(a.Windows) != 2 || *a.Windows[0].DurationMS != five || *a.Windows[0].UsedPercent != 3 || *a.Windows[1].UsedPercent != 20 {
			t.Fatalf("session %s: account must stay at weekly 20%% with a 5-hour window: %+v", a.SourceSessionID, a.Windows)
		}
	}
	// The stored idle row keeps the time its limits were first seen.
	var observed int64
	if err := s.db.QueryRowContext(ctx, `SELECT json_extract(account,'$.observed_at') FROM usage_native WHERE session_name='wb-5'`).Scan(&observed); err != nil || observed != start.UnixMilli() {
		t.Fatalf("idle limits time %d (%v), want first seen %d", observed, err, start.UnixMilli())
	}
	// New API activity in the idle session dates its limits anew.
	resumed := last.Add(5 * time.Second)
	save("wb-5", "idle", resumed, 900, weekly(21))
	if err := s.db.QueryRowContext(ctx, `SELECT json_extract(account,'$.observed_at') FROM usage_native WHERE session_name='wb-5'`).Scan(&observed); err != nil || observed != resumed.UnixMilli() {
		t.Fatalf("active limits time %d (%v), want %d", observed, err, resumed.UnixMilli())
	}
}

// A Claude run that ends inside a live pane (SessionEnd) is reported as
// stopped; a resume of the same run (a later SessionStart) is running again.
func TestNativeUsageMarksAClaudeRunStoppedBySessionEnd(t *testing.T) {
	ctx := context.Background()
	s, en := limitsStore(t)
	native := ops.UsageID("native", "run")
	at := time.Now().Add(-time.Minute)
	api := int64(10)
	row := ops.UsageSession{ID: native, NativeID: native, Name: "wb-2", Harness: "claude", AuthMode: "unknown", Source: "claude-statusline", ObservedAt: at.UnixMilli(), UsageCounters: ops.UsageCounters{APIDurationMS: &api}}
	if err := s.SaveNativeUsage(ctx, en, ops.UsageReport{Version: 1, ObservedAt: at.UnixMilli(), Sessions: []ops.UsageSession{row}}, at); err != nil {
		t.Fatal(err)
	}
	event := func(kind string, when time.Time) {
		t.Helper()
		hook := ops.UsageSession{ID: native, NativeID: native, Name: "wb-2", Harness: "claude", AuthMode: "unknown", Source: "claude-hook", ObservedAt: when.UnixMilli()}
		e := ops.UsageEvent{ID: ops.UsageID("event", kind, when.String()), NativeID: native, Kind: kind, Source: "claude-hook", ObservedAt: when.UnixMilli()}
		if err := s.SaveNativeUsage(ctx, en, ops.UsageReport{Version: 1, ObservedAt: when.UnixMilli(), Sessions: []ops.UsageSession{hook}, Events: []ops.UsageEvent{e}}, when); err != nil {
			t.Fatal(err)
		}
	}
	read := func(now time.Time) ops.UsageSession {
		t.Helper()
		started := at.Add(-time.Hour).UnixMilli()
		report := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true, Sessions: []ops.UsageSession{{ID: ops.UsageID("host", "wb-2"), Name: "wb-2", AuthMode: "unknown", Source: "session-host", StartedAt: &started, ObservedAt: now.UnixMilli()}}}
		if err := s.NativeUsage(ctx, &report, 0); err != nil {
			t.Fatal(err)
		}
		if err := report.Check(now); err != nil {
			t.Fatalf("report with a stopped run is invalid: %v", err)
		}
		return report.Sessions[0]
	}
	if got := read(at.Add(time.Second)); got.StoppedAt != nil {
		t.Fatal("a running Claude run must not be stopped")
	}
	end := at.Add(2 * time.Second)
	event("session_stopped", end)
	got := read(end.Add(time.Second))
	if got.StoppedAt == nil || *got.StoppedAt != end.UnixMilli() || got.ObservedAt < end.UnixMilli() {
		t.Fatalf("SessionEnd must stop the run: %+v", got)
	}
	event("session_started", end.Add(10*time.Second))
	if got := read(end.Add(11 * time.Second)); got.StoppedAt != nil {
		t.Fatal("a resumed run is running again")
	}
}
