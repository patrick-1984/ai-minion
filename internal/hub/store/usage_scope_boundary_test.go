package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageBucketProjectionUsesMembersAndAuthorizedWorkerScope(t *testing.T) {
	st := openTest(t)
	workers, report, now := usageFixture(t, st, 0)
	ctx := t.Context()
	used := 30.0
	window := ops.UsageWindow{Scope: "general", Period: "rolling", UsedPercent: &used}
	account := func(label, mode string) ops.UsageAccount {
		return ops.UsageAccount{ID: ops.UsageID("fixture-account", label), Label: label, Provider: "Anthropic", AuthMode: mode, Source: "claude-statusline", ObservedAt: report.ObservedAt, Windows: []ops.UsageWindow{window}}
	}
	a := account("Shared subscription", "subscription")
	b := account("Unlinked subscription", "subscription")
	key := account("API credential", "api")
	key.Source, key.Windows = "native-api-identity", nil
	session := func(name string, a ops.UsageAccount) ops.UsageSession {
		mode := a.AuthMode
		if mode == "" {
			mode = "unknown"
		}
		return ops.UsageSession{ID: ops.UsageID("fixture-session", name), Name: name, AccountID: a.ID, AuthMode: mode, ObservedAt: report.ObservedAt, Source: "test"}
	}
	report.Accounts = []ops.UsageAccount{a, b, key}
	report.Sessions = []ops.UsageSession{
		session("alice-subscription-one", a), session("alice-subscription-two", a), session("alice-api", key), session("alice-unknown", ops.UsageAccount{}),
	}
	tokens := int64(31)
	report.Sessions[2].InputTokens = &tokens
	apiSession := report.Sessions[2]
	report.Events = []ops.UsageEvent{{Seq: 1, ID: ops.UsageID("fixture-event", "api"), SessionID: apiSession.ID,
		AccountID: key.ID, Kind: "task_finished", Source: "fixture", ObservedAt: report.ObservedAt}}
	if err := st.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, report, now); err != nil {
		t.Fatal(err)
	}
	report.Events = nil
	report.Accounts = []ops.UsageAccount{a}
	report.Sessions = []ops.UsageSession{session("bob-other-session", a)}
	if err := st.SaveUsage(ctx, workers[1].ID, workers[1].OwnerID, report, now); err != nil {
		t.Fatal(err)
	}

	read := func(scope []ops.WorkerInfo, owner string, args ops.UsageListArgs) ops.UsageListResult {
		t.Helper()
		out, err := st.ReadUsage(ctx, scope, owner, args, now)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	move := func(owner string, args ops.UsageBucketSetArgs) ops.UsageBucketSetResult {
		t.Helper()
		out, err := st.SetUsageBucket(ctx, owner, workers, args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	save := func(owner, kind, label string) string {
		t.Helper()
		return move(owner, ops.UsageBucketSetArgs{Action: "save", Kind: kind, Label: label}).ID
	}
	alice := workers[0].OwnerID
	bob := workers[1].OwnerID
	sessionBucket := save(alice, "session", "Work")
	providerBucket := save(alice, "provider", "Work")
	temporary := save(alice, "session", "Temporary")
	bobProvider := save(bob, "provider", "Own shared account")
	move(bob, ops.UsageBucketSetArgs{Action: "assign", Kind: "provider", BucketID: bobProvider, Members: []string{a.ID}})
	aliceSessions := read(workers[:1], alice, ops.UsageListArgs{Section: "sessions"}).Sessions
	var selected []string
	var unknownID, hubAPISessionID string
	for _, row := range aliceSessions {
		if row.AccountID != "" {
			selected = append(selected, row.ID)
		} else {
			unknownID = row.ID
		}
		if row.AccountID == key.ID {
			hubAPISessionID = row.ID
		}
	}
	move(alice, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: temporary, WorkerIDs: []string{workers[0].ID}})
	move(alice, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: sessionBucket, Members: selected})
	move(alice, ops.UsageBucketSetArgs{Action: "assign", Kind: "provider", BucketID: providerBucket, Members: []string{b.ID, key.ID}})
	if got := read(workers, alice, ops.UsageListArgs{Section: "sessions", BucketID: temporary}).Sessions; len(got) != 1 || got[0].Name != "alice-unknown" {
		t.Fatalf("selected move did not leave only the unrelated identity: %+v", got)
	}
	if got := read(workers, alice, ops.UsageListArgs{Section: "sessions", BucketID: sessionBucket}).Sessions; len(got) != 3 {
		t.Fatalf("selected move from another bucket had %d sessions, want 3", len(got))
	}
	expectAccounts := func(bucket string, subscriptionsOnly bool, want ...string) {
		t.Helper()
		got := read(workers, alice, ops.UsageListArgs{Section: "accounts", BucketID: bucket, SubscriptionOnly: subscriptionsOnly})
		ids := make([]string, 0, len(got.Accounts))
		for _, row := range got.Accounts {
			ids = append(ids, row.ID)
		}
		slices.Sort(ids)
		slices.Sort(want)
		if !slices.Equal(ids, want) {
			t.Errorf("bucket %s, subscriptions-only=%t: accounts %v, want %v", bucket, subscriptionsOnly, ids, want)
		}
	}
	expectAccounts(sessionBucket, false, a.ID, key.ID)
	expectAccounts(sessionBucket, true, a.ID)
	expectAccounts(providerBucket, false, b.ID, key.ID)
	expectAccounts(providerBucket, true, b.ID)
	expectAccounts(temporary, false)
	for _, bucket := range []string{sessionBucket, providerBucket} {
		got := read(workers, alice, ops.UsageListArgs{Section: "overview", BucketID: bucket}).Overview
		if got == nil || got.Accounts != 1 || got.WithoutWindows != 0 {
			t.Errorf("mixed bucket %s did not count its single linked subscription: %+v", bucket, got)
		}
	}
	if got := read(workers, alice, ops.UsageListArgs{Section: "sessions", BucketID: providerBucket}).Sessions; len(got) != 1 || got[0].Name != "alice-api" {
		t.Errorf("provider bucket inverse join: %+v", got)
	}
	gotHistory := read(workers, alice, ops.UsageListArgs{Section: "history", BucketID: providerBucket}).History
	if len(gotHistory) != 1 || gotHistory[0].SessionID != hubAPISessionID {
		t.Errorf("provider bucket inverse history join: %+v", gotHistory)
	}
	gotEvents := read(workers, alice, ops.UsageListArgs{Section: "events", BucketID: providerBucket}).Events
	if !slices.ContainsFunc(gotEvents, func(e ops.UsageEventRow) bool { return e.Kind == "task_finished" && e.SessionID == hubAPISessionID }) {
		t.Errorf("provider bucket inverse event join: %+v", gotEvents)
	}
	if got := read(workers[1:], alice, ops.UsageListArgs{Section: "accounts", BucketID: sessionBucket}).Accounts; len(got) != 0 {
		t.Errorf("a visible same-account observation opened a bucket whose sessions were outside the selected scope: %+v", got)
	}
	move(alice, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", Members: []string{unknownID}})
	if got := read(workers[:1], alice, ops.UsageListArgs{Section: "sessions", BucketID: "unassigned"}).Sessions; len(got) != 1 || got[0].ID != unknownID || got[0].AccountID != "" {
		t.Errorf("an unknown link did not stay unassigned: %+v", got)
	}

	// Moving an active terminal onto another native account must affect
	// current-session attribution without rewriting an earlier hourly sample.
	later := now.Add(time.Hour)
	apiSession.AccountID, apiSession.AuthMode, apiSession.ObservedAt = a.ID, a.AuthMode, later.UnixMilli()
	if err := st.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, ops.UsageReport{
		Version: ops.UsageVersion, ObservedAt: later.UnixMilli(), Sessions: []ops.UsageSession{apiSession},
	}, later); err != nil {
		t.Fatal(err)
	}
	if got := read(workers, alice, ops.UsageListArgs{Section: "sessions", BucketID: providerBucket}).Sessions; len(got) != 0 {
		t.Errorf("current sessions stayed linked to their prior API credential: %+v", got)
	}
	if got := read(workers, alice, ops.UsageListArgs{Section: "history", BucketID: providerBucket}).History; len(got) != 1 || got[0].ObservedAt != now.UnixMilli() {
		t.Errorf("account change rewrote an earlier hourly attribution: %+v", got)
	}
	for _, args := range []ops.UsageListArgs{
		{Section: "accounts", BucketID: sessionBucket},
		{Section: "accounts", BucketID: providerBucket},
	} {
		if got := read(workers, bob, args); len(got.Accounts) != 0 {
			t.Errorf("domain worker visibility opened another owner's bucket: %+v", got)
		}
	}
	if got := read(workers, bob, ops.UsageListArgs{Section: "accounts", BucketID: bobProvider}).Accounts; len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("another owner's moves displaced private provider membership: %+v", got)
	}
	if _, err := st.SetUsageBucket(ctx, alice, workers, ops.UsageBucketSetArgs{Action: "assign", Kind: "provider", BucketID: sessionBucket, Members: []string{a.ID}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("allowed cross-type membership in session bucket: %v", err)
	}
}

func TestUsagePassiveCollectorFreshnessSurvivesIdleWorkerScans(t *testing.T) {
	st := openTest(t)
	workers, report, observed := usageFixture(t, st, 1)
	report.Accounts[0].Provider, report.Accounts[0].Source = "Anthropic", "claude-statusline"
	report.Accounts[0].SourceSessionID = report.Sessions[0].ID
	zero := 0.0
	reset := observed.Add(2 * time.Hour).UnixMilli()
	report.Accounts[0].Windows = []ops.UsageWindow{{Scope: "general", Period: "rolling", UsedPercent: &zero, ResetsAt: &reset}}
	ctx := t.Context()
	w := workers[0]
	if err := st.SaveUsage(ctx, w.ID, w.OwnerID, report, observed); err != nil {
		t.Fatal(err)
	}
	later := observed.Add(25 * time.Minute)
	report.ObservedAt = later.UnixMilli()
	report.Sessions[0].ObservedAt = report.ObservedAt
	report.Accounts = nil // The Worker still sees its tty, but the native account is idle.
	if err := st.SaveUsage(ctx, w.ID, w.OwnerID, report, later); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReadUsage(ctx, workers[:1], w.OwnerID, ops.UsageListArgs{Section: "accounts"}, later)
	if err != nil || len(got.Accounts) != 1 {
		t.Fatalf("idle account: %+v, %v", got, err)
	}
	row := got.Accounts[0]
	if row.ObservedAt != observed.UnixMilli() || row.Collector == nil || row.Collector.Mode != "passive_events" ||
		row.Collector.CanStart || row.Collector.NextDue != nil || row.Collector.LastSuccess == nil || *row.Collector.LastSuccess != observed.UnixMilli() {
		t.Errorf("regular Worker activity advanced passive account freshness: %+v, collector %+v", row, row.Collector)
	}
	view, err := st.ReadUsage(ctx, workers[:1], w.OwnerID, ops.UsageListArgs{Section: "overview"}, later)
	if err != nil || view.Overview == nil || len(view.Overview.Groups) != 1 {
		t.Fatalf("idle overview: %+v, %v", view, err)
	}
	group := view.Overview.Groups[0]
	if group.Known != 0 || group.Stale != 1 || group.Unknown != 0 || group.MeanUsedPercent != nil {
		t.Errorf("old real zero was restamped or counted as unknown: %+v", group)
	}
}
