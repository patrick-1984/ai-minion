package ops

import (
	"fmt"
	"strings"
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

// Valid quota detail never stops a whole report: when it alone makes the
// report too large, the windows are dropped and the sessions are kept.
func TestUsageSanitizeDropsOversizedQuotaDetailKeepsTheReport(t *testing.T) {
	now := time.Now()
	at := now.UnixMilli()
	var windows []UsageWindow
	for i := range 32 {
		windows = append(windows, UsageWindow{Scope: fmt.Sprintf("model-%02d-%s", i, strings.Repeat("x", 60)), Period: "rolling", UsedPercent: new(20.)})
	}
	account := UsageAccount{ID: UsageID("big", "account"), Provider: "Anthropic", AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, Windows: windows}
	r := UsageReport{Version: UsageVersion, ObservedAt: at, Accounts: []UsageAccount{account}}
	for i := range 200 {
		r.Sessions = append(r.Sessions, UsageSession{ID: UsageID("big", fmt.Sprint(i)), Name: fmt.Sprint("s", i), AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, AccountID: account.ID, Windows: windows})
	}
	if err := r.Check(now); err == nil {
		t.Fatal("the fixture must exceed the transport limit")
	}
	got := r.Sanitize()
	if err := got.Check(now); err != nil {
		t.Fatalf("sanitized report: %v", err)
	}
	if len(got.Sessions) != 200 || len(got.Accounts) != 1 || len(got.Accounts[0].Windows) != 0 || len(got.Sessions[0].Windows) != 0 {
		t.Fatalf("kept %d sessions, account windows %d, session windows %d", len(got.Sessions), len(got.Accounts[0].Windows), len(got.Sessions[0].Windows))
	}
	if len(r.Sessions[0].Windows) != 32 {
		t.Fatal("Sanitize changed its input")
	}
}

// The aggregate API-key account never carries quota windows, and trimming an
// oversized report never touches a subscription's measured limits.
func TestUsageSanitizeKeepsSubscriptionWindowsAndDropsAPIOnes(t *testing.T) {
	now := time.Now()
	at := now.UnixMilli()
	win := []UsageWindow{{Scope: "general", Period: "rolling", UsedPercent: new(60.)}}
	var big []UsageWindow
	for i := range 32 {
		big = append(big, UsageWindow{Scope: fmt.Sprintf("model-%02d-%s", i, strings.Repeat("x", 60)), Period: "rolling", UsedPercent: new(20.)})
	}
	sub := UsageAccount{ID: UsageID("sub"), Provider: "Anthropic", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: at, Windows: win}
	api := UsageAccount{ID: UsageID("api"), Provider: "Anthropic", AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, Windows: win}
	r := UsageReport{Version: UsageVersion, ObservedAt: at, Accounts: []UsageAccount{sub, api}}
	for i := range 200 {
		r.Sessions = append(r.Sessions, UsageSession{ID: UsageID("s", fmt.Sprint(i)), Name: fmt.Sprint("s", i), AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, AccountID: api.ID, Windows: big})
	}
	got := r.Sanitize()
	if len(got.Accounts) != 2 || len(got.Accounts[0].Windows) != 1 || len(got.Accounts[1].Windows) != 0 || len(got.Sessions[0].Windows) != 0 {
		t.Fatalf("sanitized accounts: %+v", got.Accounts)
	}
	if len(r.Accounts[1].Windows) != 1 {
		t.Fatal("Sanitize changed its input")
	}
}
