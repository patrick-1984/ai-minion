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

func TestUsageSummaryComparableProviderPeriodsAndUnits(t *testing.T) {
	now := time.Now()
	d := int64(5 * time.Hour / time.Millisecond)
	a, b := 20., 60.
	makeRow := func(provider, scope string, v *float64) UsageAccountRow {
		return UsageAccountRow{UsageAccount: UsageAccount{ID: UsageID("account", provider), Provider: provider, AuthMode: "subscription", ObservedAt: now.UnixMilli(), Windows: []UsageWindow{{Scope: scope, Period: "rolling", DurationMS: &d, UsedPercent: v}}}}
	}
	claude, codex := makeRow("Anthropic", "general", &a), makeRow("OpenAI", "codex.primary", &b)
	out := SummarizeUsage([]UsageAccountRow{claude, codex, codex}, now)
	any := false
	for _, g := range out.Groups {
		if g.Provider == "All subscriptions" {
			any = true
			if g.Known != 2 || g.Unknown != 0 || g.MeanUsedPercent == nil || *g.MeanUsedPercent != 40 {
				t.Fatalf("pooled comparable: %+v", g)
			}
		}
	}
	if !any {
		t.Fatal("missing comparable group")
	}
	daily := makeRow("Google", "gemini", &b)
	w := daily.Windows[0]
	w.Unit = "requests"
	other := w
	other.Unit = "tokens"
	daily.Windows = []UsageWindow{w, other}
	g := SummarizeUsage([]UsageAccountRow{daily}, now)
	if len(g.Groups) != 2 || g.Groups[0].Unknown < 0 || g.Groups[1].Unknown < 0 {
		t.Fatalf("mixed units: %+v", g)
	}
}

func TestUsageProviderAliasesShareQuotaCohort(t *testing.T) {
	now := time.Now()
	pct := 40.
	d := int64(time.Hour / time.Millisecond)
	rows := []UsageAccountRow{}
	for _, name := range []string{"OpenAI", "open ai"} {
		rows = append(rows, UsageAccountRow{UsageAccount: UsageAccount{ID: UsageID("test", name), Provider: name, AuthMode: "subscription", ObservedAt: now.UnixMilli(), Windows: []UsageWindow{{Scope: "codex.primary", Period: "rolling", DurationMS: &d, UsedPercent: &pct}}}})
	}
	o := SummarizeUsage(rows, now)
	if len(o.Groups) != 1 || o.Groups[0].Provider != "OpenAI" || o.Groups[0].Known != 2 {
		t.Fatalf("alias cohorts: %+v", o.Groups)
	}
}
