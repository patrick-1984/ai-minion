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
				g.Known != 2 || g.Stale != 1 || g.Unknown != 1 || g.MeanUsedPercent == nil || *g.MeanUsedPercent != 25 {
				t.Errorf("cross-provider weekly cohort incorrectly mixed in another scope, unit, stale or superseded data: %+v", g)
			}
		case g.Provider == "Anthropic" && g.Scope == "general" && g.Unit == "":
			foundZero = true
			if g.MeanUsedPercent == nil || *g.MeanUsedPercent != 0 || g.Known != 1 || g.Unknown != 0 {
				t.Errorf("known zero was lost or mixed with a different native unit: %+v", g)
			}
		}
	}
	if !foundMean || !foundZero {
		t.Fatalf("required cohort missing: %+v", out.Groups)
	}
}

func TestUsageCoverageExcludesWindowsAbsentFromFreshReports(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	fiveHours := (5 * time.Hour).Milliseconds()
	week := (7 * 24 * time.Hour).Milliseconds()
	used := 20.
	short := UsageWindow{Scope: "general", Period: "rolling", DurationMS: &fiveHours, UsedPercent: &used}
	long := UsageWindow{Scope: "general", Period: "rolling", DurationMS: &week, UsedPercent: &used}
	unknown := short
	unknown.UsedPercent = nil
	account := func(id string, at time.Time, windows ...UsageWindow) UsageAccount {
		return UsageAccount{ID: UsageID("quota-coverage", id), Provider: "Anthropic", AuthMode: "subscription", ObservedAt: at.UnixMilli(), Windows: windows}
	}
	discovered := account("discovered", now)
	discovered.Source = "native-account"
	accounts := []UsageAccount{
		account("both", now, short, long),
		account("weekly", now.Add(-time.Minute), short, long),
		account("weekly", now, long),
		account("none", now),
		discovered,
		account("partial", now, unknown),
		account("aged", now.Add(-time.Hour), short),
		account("aged-without-window", now.Add(-time.Hour), long),
	}
	want := UsageMeterCoverage{Known: 1, Stale: 1, Unknown: 3}
	t.Run("subscriptions", func(t *testing.T) {
		var rows []UsageAccountRow
		for _, a := range accounts {
			rows = append(rows, UsageAccountRow{UsageAccount: a})
		}
		for _, g := range SummarizeUsage(rows, now).Groups {
			if g.DurationMS != nil && *g.DurationMS == fiveHours {
				if got := (UsageMeterCoverage{Known: g.Known, Stale: g.Stale, Unknown: g.Unknown}); got != want {
					t.Fatalf("5-hour coverage = %+v, want %+v", got, want)
				}
				return
			}
		}
		t.Fatal("missing 5-hour group")
	})
	t.Run("readback", func(t *testing.T) {
		meters, _ := ProviderMeters(accounts, now)
		for _, m := range meters {
			if m.DurationMS != nil && *m.DurationMS == fiveHours {
				if m.Coverage != want {
					t.Fatalf("5-hour coverage = %+v, want %+v", m.Coverage, want)
				}
				return
			}
		}
		t.Fatal("missing 5-hour meter")
	})
}

func TestUsageReportRestrictsUpstreamWindowsToCustomEndpointAPI(t *testing.T) {
	now := time.Now()
	at := now.UnixMilli()
	windows := []UsageWindow{{Scope: "general", Period: "rolling", UsedPercent: new(20.)}}
	a := UsageAccount{ID: UsageID("upstream", "account"), Provider: "Anthropic", AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, Windows: windows}
	s := UsageSession{ID: UsageID("upstream", "session"), Name: "work", AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, AccountID: a.ID, Windows: windows}
	r := UsageReport{Version: UsageVersion, ObservedAt: at, Accounts: []UsageAccount{a}, Sessions: []UsageSession{s}}
	if err := r.Check(now); err != nil {
		t.Fatalf("custom endpoint report: %v", err)
	}
	if meters, _ := ProviderMeters(r.Accounts, now); len(meters) != 0 {
		t.Fatal("custom endpoint counted toward subscription capacity")
	}
	r.Accounts[0].CustomEndpoint = false
	if err := r.Check(now); err == nil {
		t.Fatal("provider API account with upstream windows was accepted")
	}
	r.Accounts[0].CustomEndpoint = true
	r.Sessions[0].AuthMode = "subscription"
	if err := r.Check(now); err == nil {
		t.Fatal("subscription session with upstream windows was accepted")
	}
}
