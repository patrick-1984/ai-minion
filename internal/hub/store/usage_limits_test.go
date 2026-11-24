package store

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func limitWindow(duration time.Duration, used float64, reset time.Time) ops.UsageWindow {
	d, r := duration.Milliseconds(), reset.UnixMilli()
	return ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &d, UsedPercent: &used, ResetsAt: &r}
}

func checkFormulaLimits(t *testing.T, a ops.UsageAccount, want time.Time) {
	t.Helper()
	if len(a.Windows) != 2 || *a.Windows[0].DurationMS != (5*time.Hour).Milliseconds() || *a.Windows[0].UsedPercent != 3 || *a.Windows[1].UsedPercent != 20 {
		t.Fatalf("account must stay at weekly 20%% with 5-hour 3%%: %+v", a.Windows)
	}
	if a.ObservedAt != want.UnixMilli() {
		t.Fatalf("limits time %d, want %d", a.ObservedAt, want.UnixMilli())
	}
}

// One account seen by two sessions (or two Workers): an idle one re-sends an
// old weekly 4% without a 5-hour window, an active one reports 20% and a
// 5-hour window. Whatever arrives last, the account keeps the active reading.
func TestUsageAccountLimitsMergePerWindowOnSaveAndAcrossWorkers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	weekReset, fiveReset := now.Add(5*24*time.Hour), now.Add(3*time.Hour)
	id := ops.UsageID("account", "subscription", "formula")
	account := func(at time.Time, windows ...ops.UsageWindow) ops.UsageAccount {
		return ops.UsageAccount{ID: id, Provider: "Anthropic", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: at.UnixMilli(), Windows: windows}
	}
	active := account(now.Add(-time.Minute), limitWindow(5*time.Hour, 3, fiveReset), limitWindow(7*24*time.Hour, 20, weekReset))
	idle := account(now, limitWindow(7*24*time.Hour, 4, weekReset))
	save := func(worker string, at time.Time, a ops.UsageAccount) {
		t.Helper()
		w := workers[0]
		if worker == workers[1].ID {
			w = workers[1]
		}
		if err := s.SaveUsage(ctx, w.ID, w.OwnerID, ops.UsageReport{Version: 1, ObservedAt: at.UnixMilli(), Accounts: []ops.UsageAccount{a}}, at); err != nil {
			t.Fatal(err)
		}
	}
	// Same Worker: the idle re-send arrives after the active report.
	save("w1", now.Add(-time.Minute), active)
	save("w1", now, idle)
	got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "accounts"}, now)
	if err != nil || len(got.Accounts) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	checkFormulaLimits(t, got.Accounts[0].UsageAccount, now)
	// Two Workers: w2 has only the idle reading, newer than w1's.
	if _, err := s.db.Exec(`DELETE FROM usage_accounts; DELETE FROM usage_workers`); err != nil {
		t.Fatal(err)
	}
	save("w1", now.Add(-time.Minute), active)
	save("w2", now, idle)
	for _, section := range []string{"accounts", "overview"} {
		got, err = s.ReadUsage(ctx, workers, "alice", ops.UsageListArgs{Section: section}, now)
		if err != nil {
			t.Fatal(err)
		}
		if section == "accounts" {
			if len(got.Accounts) != 1 {
				t.Fatalf("accounts: %+v", got.Accounts)
			}
			checkFormulaLimits(t, got.Accounts[0].UsageAccount, now)
			continue
		}
		var five, week *ops.UsageWindowGroup
		for i, g := range got.Overview.Groups {
			switch *g.DurationMS {
			case (5 * time.Hour).Milliseconds():
				five = &got.Overview.Groups[i]
			case (7 * 24 * time.Hour).Milliseconds():
				week = &got.Overview.Groups[i]
			}
		}
		if five == nil || five.Known != 1 || week == nil || week.MeanUsedPercent == nil || *week.MeanUsedPercent != 20 {
			t.Fatalf("overview: %+v", got.Overview.Groups)
		}
	}
	// The readback reads the same merged limits.
	accounts, err := readUsageAccounts(ctx, s.db, []usageSourcePair{{id, "w1"}, {id, "w2"}})
	if err != nil || len(accounts) != 1 {
		t.Fatalf("%+v %v", accounts, err)
	}
	checkFormulaLimits(t, accounts[0], now)
}

// History totals include ended sessions; the live totals do not.
func TestUsageAccountTotalsSplitLiveFromHistory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	api := ops.UsageAccount{ID: ops.UsageID("account", "api", "split"), Provider: "Anthropic", AuthMode: "api", ObservedAt: now.UnixMilli(), Source: "test"}
	session := func(name string, cost float64) ops.UsageSession {
		return ops.UsageSession{ID: ops.UsageID("session", name), Name: name, AuthMode: "api", AccountID: api.ID, ObservedAt: now.UnixMilli(), Source: "test",
			UsageCounters: ops.UsageCounters{EstimatedCostUSD: &cost}}
	}
	r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true, Accounts: []ops.UsageAccount{api}, Sessions: []ops.UsageSession{session("ended", 1000), session("live", 2.5)}}
	if err := s.SaveUsage(ctx, "w1", "alice", r, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Minute)
	r.ObservedAt, r.Sessions = later.UnixMilli(), r.Sessions[1:]
	r.Accounts[0].ObservedAt = later.UnixMilli()
	r.Sessions[0].ObservedAt = later.UnixMilli()
	if err := s.SaveUsage(ctx, "w1", "alice", r, later); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "accounts"}, later)
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].Totals == nil {
		t.Fatalf("%+v %v", got, err)
	}
	tt := got.Accounts[0].Totals
	if tt.Sessions != 2 || *tt.EstimatedCostUSD != 1002.5 || tt.LiveSessions != 1 || tt.LiveEstimatedCostUSD == nil || *tt.LiveEstimatedCostUSD != 2.5 || tt.LiveInputTokens != nil {
		t.Fatalf("totals: %+v", tt)
	}
}

// A re-save never reopens an ended session unless it has the same start.
func TestUsageResaveRevivesAnEndedSessionOnlyWithTheSameStart(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	started := now.Add(-time.Hour).UnixMilli()
	row := ops.UsageSession{ID: ops.UsageID("session", "pane"), Name: "wb-5", AuthMode: "unknown", Source: "test", StartedAt: &started, ObservedAt: now.UnixMilli()}
	at := now
	save := func(r ops.UsageSession, complete bool) {
		t.Helper()
		at = at.Add(time.Second)
		r.ObservedAt = at.UnixMilli()
		report := ops.UsageReport{Version: 1, ObservedAt: at.UnixMilli(), Complete: complete}
		if r.ID != "" {
			report.Sessions = []ops.UsageSession{r}
		}
		if err := s.SaveUsage(ctx, "w1", "alice", report, at); err != nil {
			t.Fatal(err)
		}
	}
	ended := func() bool {
		t.Helper()
		got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "sessions"}, at)
		if err != nil || len(got.Sessions) != 1 {
			t.Fatalf("%+v %v", got, err)
		}
		return got.Sessions[0].StoppedAt != nil
	}
	save(row, true)
	save(ops.UsageSession{}, true) // absent from a complete scan: ended
	if !ended() {
		t.Fatal("absent session must end")
	}
	other := row
	otherStart := started + 1000
	other.StartedAt = &otherStart
	save(other, true)
	if !ended() {
		t.Fatal("a different start must not revive the ended session")
	}
	noStart := row
	noStart.StartedAt = nil
	save(noStart, true)
	if !ended() {
		t.Fatal("a re-save without a start must not revive the ended session")
	}
	save(row, true)
	if ended() {
		t.Fatal("the same host session seen again is running")
	}
	// A Worker report of a stopped Claude run ends it in place.
	stopped := row
	stop := at.Add(time.Second).UnixMilli()
	stopped.StoppedAt = &stop
	save(stopped, true)
	if !ended() {
		t.Fatal("a reported native stop must end the session")
	}
}
