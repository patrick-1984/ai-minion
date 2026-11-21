package ops

import (
	"testing"
	"time"
)

func TestUsageWeeklyMeanUsesLatestComparableQuotaWithoutLosingZero(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	week := (7 * 24 * time.Hour).Milliseconds()
	day := (24 * time.Hour).Milliseconds()
	tomorrow := now.Add(84 * time.Hour).UnixMilli()
	earlier := now.Add(-time.Second).UnixMilli()
	zero, fast, middle := 0., 85., 50.
	window := func(scope, unit string, duration *int64, used *float64) UsageWindow {
		return UsageWindow{Scope: scope, Unit: unit, Period: "rolling", DurationMS: duration, ResetsAt: &tomorrow, UsedPercent: used}
	}
	account := func(id, provider, auth string, at int64, windows ...UsageWindow) UsageAccountRow {
		return UsageAccountRow{UsageAccount: UsageAccount{ID: UsageID("quota-boundary", id), Provider: provider, AuthMode: auth, ObservedAt: at, Windows: windows}}
	}
	claude := account("A", "Anthropic", "subscription", now.UnixMilli(), window("general", "", &week, &zero), window("model-only", "", &week, &fast))
	old := account("A", "Anthropic", "subscription", earlier, window("general", "", &week, &fast))
	codex := account("B", "OpenAI", "subscription", now.UnixMilli(), window("codex.secondary", "", &week, &middle), window("codex.primary", "", &day, &fast))
	expired := account("C", "OpenAI", "subscription", now.UnixMilli(), window("codex.secondary", "", &week, &fast))
	expired.Windows[0].ResetsAt = &earlier
	unknown := account("D", "OpenAI", "subscription", now.UnixMilli(), window("codex.secondary", "", &week, nil))
	otherUnits := account("E", "Anthropic", "subscription", now.UnixMilli(), window("general", "USD", &week, &fast))
	api := account("F", "OpenAI", "api", now.UnixMilli())
	out := SummarizeUsage([]UsageAccountRow{api, old, claude, codex, expired, unknown, otherUnits}, now)
	if out.Accounts != 5 || out.WithoutWindows != 0 {
		t.Errorf("latest account population: %+v", out)
	}
	foundMean := false
	foundZero := false
	for _, g := range out.Groups {
		switch {
		case g.Provider == "All subscriptions":
			foundMean = true
			if g.Scope != "General coding allowance" || g.Unit != "" || g.DurationMS == nil || *g.DurationMS != week ||
				g.Known != 2 || g.Stale != 1 || g.Unknown != 2 || g.MeanUsedPercent == nil || *g.MeanUsedPercent != 25 {
				t.Errorf("cross-provider weekly cohort incorrectly mixed in another scope, unit, stale or superseded data: %+v", g)
			}
		case g.Provider == "Anthropic" && g.Scope == "general" && g.Unit == "":
			foundZero = true
			if g.MeanUsedPercent == nil || *g.MeanUsedPercent != 0 || g.Known != 1 || g.Unknown != 1 {
				t.Errorf("known zero was lost or mixed with a different native unit: %+v", g)
			}
		}
	}
	if !foundMean || !foundZero {
		t.Fatalf("required cohort missing: %+v", out.Groups)
	}
}
