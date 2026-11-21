package ops

import (
	"math"
	"testing"
	"time"
)

func TestUsageSummaryDeduplicationAndCoverage(t *testing.T) {
	now := time.Now()
	v := float64(50)
	d := int64(time.Hour / time.Millisecond)
	reset := now.Add(30 * time.Minute).UnixMilli()
	w := UsageWindow{Scope: "general", Period: "fixed", DurationMS: &d, ResetsAt: &reset, UsedPercent: &v}
	row := func(id, auth string, age time.Duration, ws ...UsageWindow) UsageAccountRow {
		return UsageAccountRow{UsageAccount: UsageAccount{ID: UsageID("account", id), Provider: "test", AuthMode: auth, ObservedAt: now.Add(-age).UnixMilli(), Windows: ws}}
	}
	cases := []UsageAccountRow{row("one", "subscription", time.Minute, w), row("one", "subscription", 0, w), row("two", "subscription", 20*time.Minute, w), row("three", "subscription", 0), row("four", "api", 0), row("five", "unknown", 0)}
	got := SummarizeUsage(cases, now)
	if got.Accounts != 3 || got.WithoutWindows != 1 || len(got.Groups) != 1 {
		t.Fatalf("summary: %+v", got)
	}
	g := got.Groups[0]
	if g.Known != 1 || g.Stale != 1 || g.Unknown != 1 || g.MeanUsedPercent == nil || *g.MeanUsedPercent != 50 {
		t.Fatalf("coverage: %+v", g)
	}
	if p := w.Pace(now); p != "on_pace" {
		t.Fatalf("pace %s", p)
	}
	v = 0
	if w.Pace(now) != "below" {
		t.Fatal("zero mistaken for unknown")
	}
	v = 95
	if w.Pace(now) != "ahead" {
		t.Fatal("fast consumption not recognized")
	}
	for _, val := range []float64{-1, 101, math.NaN()} {
		v = val
		if w.Pace(now) != "unknown" {
			t.Fatal("invalid percentage")
		}
	}
	w.UsedPercent = nil
	if w.Pace(now) != "unknown" {
		t.Fatal("missing percentage")
	}
}
