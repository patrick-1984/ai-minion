package web

import (
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func usageCardOf(t *testing.T, body, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, heading)
	if !ok {
		t.Fatalf("no card %q in %s", heading, body)
	}
	card, _, _ := strings.Cut(rest, "</article>")
	return card
}

// A missing 5-hour window is "no 5-hour limit" only when the account's
// freshest reports are active ones; otherwise it is unknown. A passive
// collector with a recent sample is collecting, not waiting.
func TestUsageFiveHourAbsenceUnknownAndPassiveCollector(t *testing.T) {
	now := time.Now()
	at := now.UnixMilli()
	week := (7 * 24 * time.Hour).Milliseconds()
	reset := now.Add(48 * time.Hour).UnixMilli()
	used := 4.0
	weekly := ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &week, ResetsAt: &reset, UsedPercent: &used}
	old := now.Add(-2 * time.Hour).UnixMilli()
	row := func(label string, observed int64, last *int64) ops.UsageAccountRow {
		return ops.UsageAccountRow{WorkerID: "build", UsageAccount: ops.UsageAccount{ID: ops.UsageID("account", label), Provider: "Anthropic", Label: label,
			AuthMode: "subscription", Source: "claude-statusline", ObservedAt: observed, Windows: []ops.UsageWindow{weekly}},
			Collector: &ops.UsageCollector{Status: "passive", Mode: "passive_events", LastSuccess: last}}
	}
	accounts := []ops.UsageAccountRow{row("Active twenty-x", at, &at), row("Idle only", old, &old)}
	connect(t, usagePageFixtures().on(ops.OpUsageList, func(args map[string]any) (any, error) {
		return ops.UsageListResult{Version: 1, AsOf: at, Accounts: accounts}, nil
	}))
	srv := adminServer(t)
	for _, view := range []string{"overview", "accounts"} {
		body := mainOf(t, get(t, srv, "/usage?view="+view, "X-Test-Session", userHeader).Body.String())
		active := usageCardOf(t, body, "Anthropic · Active twenty-x")
		mustHave(t, active, "No 5-hour window reported")
		mustNotHave(t, active, "5-hour limit unknown")
		idle := usageCardOf(t, body, "Anthropic · Idle only")
		mustHave(t, idle, "5-hour limit unknown")
		mustNotHave(t, idle, "No 5-hour window reported")
		if view == "accounts" {
			mustHave(t, active, "Collector · Passive · from Claude status line (last ")
			mustNotHave(t, active, "Waiting for native observations")
			mustHave(t, idle, "Collector · Waiting for native observations")
		}
	}
}

func TestUsageAccountTotalsShowLiveApartFromHistory(t *testing.T) {
	at := time.Now().UnixMilli()
	live, history := 2.5, 1624.75
	api := ops.UsageAccountRow{WorkerID: "build", UsageAccount: ops.UsageAccount{ID: ops.UsageID("account", "api"), Provider: "DeepSeek", Label: "Key", AuthMode: "api", Source: "test", ObservedAt: at},
		Totals: &ops.UsageAccountTotals{Sessions: 13, EstimatedCostUSD: &history, LiveSessions: 1, LiveEstimatedCostUSD: &live}}
	connect(t, usagePageFixtures().on(ops.OpUsageList, func(args map[string]any) (any, error) {
		return ops.UsageListResult{Version: 1, AsOf: at, Accounts: []ops.UsageAccountRow{api}}, nil
	}))
	body := mainOf(t, get(t, adminServer(t), "/usage?view=accounts", "X-Test-Session", userHeader).Body.String())
	liveAt := strings.Index(body, "Running now · 1 session<")
	historyAt := strings.Index(body, "History · last 90 days · 13 sessions")
	if liveAt < 0 || historyAt < liveAt {
		t.Fatalf("live totals must come first and apart from the history: %s", body)
	}
	mustHave(t, body[liveAt:historyAt], "$2.5000")
	mustNotHave(t, body[liveAt:historyAt], "$1624.7500")
	mustHave(t, body[historyAt:], "$1624.7500", "Includes ended sessions", "Not current spend")
}
