package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func usagePageFixtures() *fakeOps {
	now := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC)
	at := now.UnixMilli()
	stale := now.Add(-25 * time.Minute).UnixMilli()
	duration := int64(5 * time.Hour / time.Millisecond)
	reset := now.Add(3 * time.Hour).UnixMilli()
	percent := 47.5
	tokens := int64(8100)
	sessionID := ops.UsageID("session", "example")
	build := ops.WorkerInfo{ID: "build", Name: "Build workstation", Owner: "alice", Online: true}
	laptop := ops.WorkerInfo{ID: "laptop", Name: "Travel laptop", Owner: "bob"}
	pending := ops.WorkerInfo{ID: "pending", Name: "New Worker", Owner: "bob", Online: true}
	account := ops.UsageAccountRow{
		UsageAccount: ops.UsageAccount{
			ID: ops.UsageID("account", "example"), Provider: "OpenAI", Label: "Personal subscription",
			AuthMode: "subscription", Source: "codex-app-server", ObservedAt: at,
			Windows: []ops.UsageWindow{{Scope: "Codex", Unit: "requests", Period: "rolling", DurationMS: &duration, ResetsAt: &reset, UsedPercent: &percent}},
		},
		WorkerID: build.ID, BucketID: "b", Alias: "Personal subscription", Position: 20,
		Collector: &ops.UsageCollector{Status: "ready", ActiveWorker: build.ID, PreferredWorker: build.ID, PreferredSession: sessionID, LastSuccess: &at, NextDue: &reset},
		Sources: []ops.UsageSource{
			{WorkerID: build.ID, Label: build.Name},
			{WorkerID: build.ID, SessionID: sessionID, Label: build.Name + " / release validation"},
		},
	}
	out := ops.UsageListResult{
		Version: 1, AsOf: at,
		Workers: []ops.UsageWorkerRow{
			{ID: build.ID, Name: build.Name, ObservedAt: &at, InventoryAt: &at, Online: true, Status: "ready", Inventory: []ops.UsageHarness{{Name: "claude", Version: "2.1.80", Status: "installed", AuthMode: "subscription", AccountStatus: "configured", Providers: []string{"Anthropic"}}, {Name: "gemini", Status: "unavailable"}}},
			{ID: laptop.ID, Name: laptop.Name, Status: "offline", ObservedAt: &stale},
			{ID: pending.ID, Name: pending.Name, Online: true, Status: "registration_required"},
		},
		Sessions: []ops.UsageSessionRow{
			{WorkerID: build.ID, LastSeen: at, BucketID: "a", UsageSession: ops.UsageSession{
				ID: sessionID, Name: "release-validation-on-build-workstation", Folder: "/workspace/synthetic/long-project-name-for-release-validation", Harness: "codex",
				Version: "0.126.0", Model: "test-model", AuthMode: "subscription", Source: "native-event", StartedAt: &at, ObservedAt: at,
				UsageCounters: ops.UsageCounters{InputTokens: &tokens},
			}},
		},
		Accounts: []ops.UsageAccountRow{
			account,
			{WorkerID: laptop.ID, UsageAccount: ops.UsageAccount{
				ID: ops.UsageID("account", "empty"), Provider: "Anthropic", Label: "Work subscription", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: stale,
			}},
			{WorkerID: build.ID, UsageAccount: ops.UsageAccount{
				ID: ops.UsageID("account", "api"), Provider: "DeepSeek", Label: "key-prefix\u20263492ba", AuthMode: "api", Source: "native-event", ObservedAt: at,
			}},
		},
		Events: []ops.UsageEventRow{
			{WorkerID: build.ID, UsageEvent: ops.UsageEvent{SessionID: sessionID, Kind: "subagent_finished", Source: "fixture", ObservedAt: at}},
		},
		History: []ops.UsageHistoryRow{
			{WorkerID: build.ID, SessionID: sessionID, Hour: now.Truncate(time.Hour).UnixMilli(), ObservedAt: at, Counters: ops.UsageCounters{InputTokens: &tokens}},
		},
	}
	overview := ops.SummarizeUsage(out.Accounts, now)
	out.Overview = &overview
	return newFakeOps().
		on(ops.OpUsageList, func(args map[string]any) (any, error) {
			result := out
			if args["subscription_only"] == true {
				result.Accounts = out.Accounts[:2]
			}
			return result, nil
		}).
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{build, laptop, pending, {ID: "unapproved", Name: "Unapproved Worker", Pending: true}}}).
		reply(ops.OpUsageBucketList, ops.UsageBucketListResult{Buckets: []ops.UsageBucket{{ID: "a", Kind: "session", Label: "Release validation"}, {ID: "b", Kind: "provider", Label: "Personal accounts"}}}).
		reply(ops.OpUsageBucketSet, ops.UsageBucketSetResult{Updated: 1}).
		reply(ops.OpUsageCollectorSet, ops.UsageCollector{Status: "ready"}).
		reply(ops.OpUsageAccountSet, ops.UsageAccountSetArgs{}).
		reply(ops.OpWorkerUsage, ops.UsageReport{Version: 1, ObservedAt: at})
}

func TestUsageViewsShowDisclosedFixtures(t *testing.T) {
	f := usagePageFixtures()
	connect(t, f)
	srv := adminServer(t)
	tests := []struct {
		view string
		want []string
	}{
		{"overview", []string{"47.5%", "Ahead of pace", "40.0%", "118.8%", "No subscription limits reported.", "1 fresh", "Stale", "requests"}},
		{"accounts", []string{"API credential", "DeepSeek", "Save collector", "passive session", "Next eligible query", "Display name and order", `value="20"`}},
		{"sessions", []string{"release-validation-on-build-workstation", "8100", "Unknown", "Move selected rows", "Move all from Worker"}},
		{"workers", []string{"Scan installed harnesses", "Claude Code", "Gemini CLI", "Anthropic · Subscription · Configured", "Last harness scan", "Monitoring acceptance required", `data-online="false">Offline`}},
		{"buckets", []string{"Release validation", "Your token buckets"}},
		{"events", []string{"Session activity", "Subagent finished", "fixture"}},
		{"history", []string{"Hourly session snapshots", "latest totals", "8100"}},
	}
	for _, test := range tests {
		for _, role := range []string{adminHeader, userHeader} {
			res := get(t, srv, "/usage?view="+test.view, "X-Test-Session", role)
			body := res.Body.String()
			if res.Code != 200 {
				t.Fatalf("%s returned %d: %s", test.view, res.Code, body)
			}
			mustHave(t, mainOf(t, body), append(test.want, "Monitoring and access", "including unshared sessions")...)
			if strings.Contains(body, "Unapproved Worker") || test.view == "overview" && strings.Contains(body, "DeepSeek") {
				t.Fatalf("%s rendered a record excluded by the selected view", test.view)
			}
		}
	}
}

func TestUsageFilterAndBucketSelections(t *testing.T) {
	f := usagePageFixtures()
	connect(t, f)
	srv := onbUser(t)
	page := onbGet(t, srv, "/usage?view=overview&bucket=b&worker=build")
	mustHave(t, page.Body.String(), `value="b" selected`, `value="build" selected`, `hx-get="/usage?bucket=b&amp;worker=build"`)
	list := f.called(ops.OpUsageList)
	if len(list) != 2 || list[0].Args["section"] != "overview" || list[1].Args["section"] != "accounts" || list[1].Args["subscription_only"] != true {
		t.Fatalf("overview did not query the subscription summary and individual subscription accounts: %+v", list)
	}
	for _, call := range list {
		if call.Args["bucket_id"] != "b" || call.Args["worker_id"] != "build" {
			t.Fatalf("missing filter: %+v", call.Args)
		}
	}
	member := ops.UsageID("session", "example")
	for _, selection := range []string{"selected", "worker", "all"} {
		res := onbPost(t, srv, "/usage/bucket", url.Values{
			"for_user": {onbUserID}, "view": {"sessions"}, "action": {"assign"}, "kind": {"session"}, "bucket": {"a"},
			"selection": {selection}, "worker": {"laptop"}, "member": {member},
			"filter_worker": {"build"}, "filter_bucket": {"unassigned"},
		})
		if got := res.Header().Get("HX-Redirect"); got != "/usage?bucket=unassigned&view=sessions&worker=build" {
			t.Errorf("%s: redirect %q, body %s", selection, got, res.Body.String())
		}
	}
	calls := f.called(ops.OpUsageBucketSet)
	if len(calls) != 3 {
		t.Fatalf("assignment calls: %+v", calls)
	}
	if a := calls[0].Args; a["worker_ids"] != nil || a["all"] != nil || len(a["members"].([]any)) != 1 || a["members"].([]any)[0] != member {
		t.Errorf("selected-only move: %+v", a)
	}
	if a := calls[1].Args; a["members"] != nil || a["all"] != nil || len(a["worker_ids"].([]any)) != 1 || a["worker_ids"].([]any)[0] != "laptop" {
		t.Errorf("Worker-wide move: %+v", a)
	}
	if a := calls[2].Args; a["members"] != nil || a["worker_ids"] != nil || a["all"] != true {
		t.Errorf("domain-wide move: %+v", a)
	}
}

func TestUsageActAsSuppressesPrivateReadsAndWrites(t *testing.T) {
	f := usagePageFixtures()
	connect(t, f)
	srv := onbServer(t, onbSessions{role: authz.RoleAdmin, target: "alice"})
	r := onbGet(t, srv, "/usage?view=accounts&bucket=b")
	if r.Code != 200 {
		t.Fatalf("GET status %d", r.Code)
	}
	body := mainOf(t, r.Body.String())
	if strings.Contains(body, "Personal accounts") || strings.Contains(body, "Save collector") || strings.Contains(body, "usage-selected") {
		t.Fatal("act-as view rendered private organization or controls")
	}
	list := f.called(ops.OpUsageList)
	if len(list) != 1 || list[0].Args["bucket_id"] != nil || len(f.called(ops.OpUsageBucketList)) != 0 {
		t.Fatalf("act-as queried private organization: list calls %+v", list)
	}
	for _, action := range []string{"bucket", "collector", "account"} {
		onbPost(t, srv, "/usage/"+action, url.Values{"for_user": {adminUserID}, "view": {"accounts"}, "account": {"example"}})
	}
	if len(f.called(ops.OpUsageBucketSet)) != 0 || len(f.called(ops.OpUsageCollectorSet)) != 0 || len(f.called(ops.OpUsageAccountSet)) != 0 {
		t.Fatal("act-as attempted a private mutation")
	}
}

func TestUsageReadOnlyInventoryAndPassiveCollectorEntryPoints(t *testing.T) {
	f := usagePageFixtures()
	connect(t, f)
	srv := onbUser(t)
	accountID := ops.UsageID("account", "example")
	sessionID := ops.UsageID("session", "example")
	r := onbPost(t, srv, "/usage/collector", url.Values{
		"for_user": {onbUserID}, "view": {"accounts"}, "account": {accountID}, "source": {"build|" + sessionID},
	})
	if r.Header().Get("HX-Redirect") != "/usage?view=accounts" {
		t.Fatalf("collector redirect %q, body %s", r.Header().Get("HX-Redirect"), r.Body.String())
	}
	onbPost(t, srv, "/usage/scan", url.Values{"for_user": {onbUserID}, "view": {"workers"}, "worker": {"build"}})
	onbPost(t, srv, "/usage/account", url.Values{
		"for_user": {onbUserID}, "view": {"accounts"}, "account": {accountID}, "label": {"Work account"}, "position": {"2"},
	})
	if calls := f.called(ops.OpUsageAccountSet); len(calls) != 1 || calls[0].Args["label"] != "Work account" ||
		calls[0].Args["position"] != float64(2) || calls[0].Args["account_id"] != accountID {
		t.Fatalf("account display settings: %+v", calls)
	}
	if calls := f.called(ops.OpUsageCollectorSet); len(calls) != 1 || calls[0].Args["worker_id"] != "build" || calls[0].Args["session_id"] != sessionID || calls[0].Args["account_id"] != accountID {
		t.Fatalf("collector calls: %+v", calls)
	}
	if calls := f.called(ops.OpWorkerUsage); len(calls) != 1 || calls[0].Args["worker_id"] != "build" || calls[0].Args["inventory"] != true {
		t.Fatalf("inventory calls: %+v", calls)
	}
}
