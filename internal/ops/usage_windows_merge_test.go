package ops

import (
	"testing"
	"time"
)

func TestMergeUsageWindowsPicksTheMostInformativeReadingPerWindow(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	five, week := (5 * time.Hour).Milliseconds(), (7 * 24 * time.Hour).Milliseconds()
	weekReset := now.Add(5 * 24 * time.Hour).UnixMilli()
	fiveReset := now.Add(2 * time.Hour).UnixMilli()
	oldFiveReset := now.Add(-time.Hour).UnixMilli()
	pct := func(v float64) *float64 { return &v }
	w := func(d int64, used float64, reset int64) UsageWindow {
		return UsageWindow{Scope: "general", Period: "rolling", DurationMS: &d, UsedPercent: pct(used), ResetsAt: &reset}
	}
	idle := []UsageWindow{w(week, 4, weekReset)}                           // an idle session's old reading
	active := []UsageWindow{w(five, 3, fiveReset), w(week, 20, weekReset)} // the active session
	for _, order := range [][][]UsageWindow{{idle, active}, {active, idle}} {
		got := MergeUsageWindows(now, order...)
		if len(got) != 2 || *got[0].DurationMS != five || *got[0].UsedPercent != 3 || *got[1].DurationMS != week || *got[1].UsedPercent != 20 {
			t.Fatalf("same reset must keep the higher reading and the five-hour window: %+v", got)
		}
	}
	// A later reset is a newer window, even with a lower percentage.
	got := MergeUsageWindows(now, []UsageWindow{w(five, 90, oldFiveReset)}, []UsageWindow{w(five, 1, fiveReset)})
	if len(got) != 1 || *got[0].UsedPercent != 1 || *got[0].ResetsAt != fiveReset {
		t.Fatalf("later reset must win: %+v", got)
	}
	// An expired window reads as reset; one over for longer than its duration is dropped.
	got = MergeUsageWindows(now, []UsageWindow{w(five, 90, oldFiveReset), w(week, 10, now.Add(-8*24*time.Hour).UnixMilli())})
	if len(got) != 1 || *got[0].UsedPercent != 0 || *got[0].ResetsAt != oldFiveReset {
		t.Fatalf("expired windows: %+v", got)
	}
}

func TestMergeableUsageAccountsNeedsTheSamePlanAndStatusLine(t *testing.T) {
	a := UsageAccount{ID: "x", AuthMode: "subscription", Source: "claude-statusline", Plan: "max"}
	if !MergeableUsageAccounts(a, a) {
		t.Fatal("same account")
	}
	b := a
	b.Plan = "pro"
	c := a
	c.Source = "native-account"
	if MergeableUsageAccounts(a, b) || MergeableUsageAccounts(a, c) {
		t.Fatal("another plan or a discovery row must not merge")
	}
}
