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
	totalCost := 1.5
	sessionID := ops.UsageID("session", "example")
	build := ops.WorkerInfo{ID: "build", Name: "Build workstation", Owner: "alice", Online: true, Ops: []string{ops.OpWorkerUsage}}
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
			}, Totals: &ops.UsageAccountTotals{Sessions: 2, InputTokens: &tokens, CostUSD: &totalCost}},
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
		{"accounts", []string{"API key", "DeepSeek", "Save collector", "passive session", "Next eligible query", "Display name and order", `value="20"`, "History · last 90 days · 2 sessions", "Running now · 0 sessions", "$1.5000", "ended in the last 90 days"}},
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
	// The second call reads the Worker inventories for the exporter notice.
	if len(list) != 2 || list[0].Args["bucket_id"] != nil || list[1].Args["bucket_id"] != nil || len(f.called(ops.OpUsageBucketList)) != 0 {
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
	if calls := f.called(ops.OpWorkerUsage); len(calls) != 1 || calls[0].Args["worker_id"] != "build" || calls[0].Args["inventory"] != true || calls[0].Args["fresh"] != true {
		t.Fatalf("expected a fresh inventory request: %+v", calls)
	}
}

func TestUsageShowsSeparateGatewaySpendAndAPICredentialDetails(t *testing.T) {
	now := time.Now()
	at := now.UnixMilli()
	pct := 112.5
	usd := 4.2
	limit := 5.
	api := ops.UsageAccountRow{UsageAccount: ops.UsageAccount{ID: ops.UsageID("fixture", "key"), Provider: "OpenRouter", Label: "My work alias", AuthMode: "api", EndpointOrigin: "https://openrouter.ai", KeyHint: "sk-or-v1-0... / 0123456789", ObservedAt: at, Source: "test"}, Alias: "My work alias"}
	result := ops.UsageListResult{Version: 1, AsOf: at, Accounts: []ops.UsageAccountRow{api}, Sessions: []ops.UsageSessionRow{{UsageSession: ops.UsageSession{ID: ops.UsageID("fixture", "budget"), Name: "gateway", Source: "claude-statusline", AuthMode: "unknown", GatewaySpend: &ops.UsageSpendBudget{Source: "claude-statusline", Period: "monthly", UsedPercent: &pct, EstimatedUsedUSD: &usd, LimitUSD: &limit, ObservedAt: at}}}}}
	f := usagePageFixtures().reply(ops.OpUsageList, result)
	connect(t, f)
	srv := adminServer(t)
	for _, view := range []struct {
		path string
		want []string
	}{{"accounts", []string{"My work alias", "sk-or-v1-0... / 0123456789"}}, {"sessions", []string{"Gateway spending budget", "112.5%", "monthly", "about five minutes"}}} {
		res := get(t, srv, "/usage?view="+view.path, "X-Test-Session", userHeader)
		if res.Code != 200 {
			t.Fatalf("%s: %d", view.path, res.Code)
		}
		mustHave(t, res.Body.String(), view.want...)
	}
}

func TestUsageWindowTitleIsHumanReadable(t *testing.T) {
	week := int64(7 * 24 * 3600000)
	five := int64(5 * 3600000)
	for _, c := range []struct {
		w    ops.UsageWindow
		want string
	}{
		{ops.UsageWindow{Scope: "codex.primary", Period: "rolling", DurationMS: &week}, "Weekly limit"},
		{ops.UsageWindow{Scope: "codex.secondary", Period: "rolling", DurationMS: &five}, "5-hour limit"},
		{ops.UsageWindow{Scope: "claude-opus", Period: "rolling", DurationMS: &week}, "claude-opus · Weekly limit"},
	} {
		if got := usageWindowTitle(c.w); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
}

func TestUsageShowsAbsentFiveHourLimitOnlyWithFreshProviderEvidence(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC)
	at := now.UnixMilli()
	week := (7 * 24 * time.Hour).Milliseconds()
	five := (5 * time.Hour).Milliseconds()
	reset := now.Add(4 * time.Hour).UnixMilli()
	zero := 0.
	weekly := ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &week, ResetsAt: &reset, UsedPercent: &zero}
	short := weekly
	short.DurationMS = &five
	unknown := short
	unknown.UsedPercent = nil
	row := func(label, provider string, at int64, windows ...ops.UsageWindow) ops.UsageAccountRow {
		return ops.UsageAccountRow{UsageAccount: ops.UsageAccount{ID: ops.UsageID("account", label), Provider: provider, Label: label,
			AuthMode: "subscription", Source: "claude-statusline", ObservedAt: at, Windows: windows}}
	}
	page := []ops.UsageAccountRow{
		row("Weekly only", "Anthropic", at, weekly),
		row("Stale weekly", "Anthropic", now.Add(-time.Hour).UnixMilli(), weekly),
		row("Undiscovered limits", "Anthropic", at),
		row("Known five-hour window", "Anthropic", at, weekly, unknown),
		row("Other provider", "OpenAI", at, weekly),
	}
	peer := row("Account on another page", "anthropic", at, weekly, short)
	overview := ops.SummarizeUsage(append(append([]ops.UsageAccountRow(nil), page...), peer), now)
	f := usagePageFixtures().on(ops.OpUsageList, func(args map[string]any) (any, error) {
		if args["section"] == "overview" {
			return ops.UsageListResult{AsOf: at, Overview: &overview}, nil
		}
		return ops.UsageListResult{AsOf: at, Accounts: page}, nil
	})
	connect(t, f)
	srv := adminServer(t)
	const note = "No 5-hour window reported"
	for _, view := range []string{"overview", "accounts"} {
		res := get(t, srv, "/usage?view="+view+"&worker=build&bucket=b&after=previous-account", "X-Test-Session", userHeader)
		if res.Code != 200 {
			t.Fatalf("%s: %d", view, res.Code)
		}
		body := mainOf(t, res.Body.String())
		if strings.Count(body, note) != 1 {
			t.Fatalf("%s: want one plan note, got %d", view, strings.Count(body, note))
		}
		_, rest, ok := strings.Cut(body, "Anthropic · Weekly only")
		card, _, _ := strings.Cut(rest, "</article>")
		if !ok || !strings.Contains(card, note) || !strings.Contains(card, "<h4>Weekly limit</h4>") || strings.Contains(card, "<h4>5-hour limit</h4>") {
			t.Fatalf("%s: weekly-only card should show the weekly limit and plan note: %s", view, card)
		}
	}
	for _, call := range f.called(ops.OpUsageList) {
		if call.Args["section"] == "overview" && (call.Args["worker_id"] != "build" || call.Args["bucket_id"] != "b") {
			t.Fatalf("five-hour peer lookup escaped its filters: %+v", call.Args)
		}
	}
}

func TestUsageExplainsMissingClaudeExporter(t *testing.T) {
	at := time.Now().UnixMilli()
	claude := func(exporter string) []ops.UsageHarness {
		return []ops.UsageHarness{{Name: "claude", Status: "installed", Exporter: exporter}}
	}
	anthropic := ops.UsageAccountRow{UsageAccount: ops.UsageAccount{ID: ops.UsageID("fixture", "a"), Provider: "Anthropic", Label: "Work", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: at},
		Collector: &ops.UsageCollector{Status: "passive", Mode: "passive_events"}}
	started := at - 3600000
	session := func(h string) ops.UsageSessionRow {
		return ops.UsageSessionRow{WorkerID: "build", LastSeen: at, UsageSession: ops.UsageSession{ID: ops.UsageID("fixture", h), Name: h + "-1", Harness: h, AuthMode: "unknown", Source: "session-host", StartedAt: &started, ObservedAt: at}}
	}
	run := func(inv []ops.UsageHarness, view string) string {
		result := ops.UsageListResult{Version: 1, AsOf: at, Accounts: []ops.UsageAccountRow{anthropic}, Sessions: []ops.UsageSessionRow{session("claude"), session("codex")},
			Workers: []ops.UsageWorkerRow{{ID: "build", Name: "Build workstation", Online: true, Status: "ready", ObservedAt: &at, InventoryAt: &at, Inventory: inv}}}
		f := usagePageFixtures().reply(ops.OpUsageList, result)
		connect(t, f)
		res := get(t, adminServer(t), "/usage?view="+view, "X-Test-Session", userHeader)
		if res.Code != 200 {
			t.Fatalf("%s: %d", view, res.Code)
		}
		return mainOf(t, res.Body.String())
	}
	const cmd = "minion worker config usage integrate claude"
	for _, view := range []string{"workers", "accounts", "sessions"} {
		body := run(claude("absent"), view)
		mustHave(t, body, cmd)
		if view == "workers" {
			mustHave(t, body, "needs the Minion status-line exporter on this Worker. Run on Build workstation:", "Then restart the Claude sessions")
		}
		if view == "accounts" {
			mustHave(t, body, "No Worker has it yet", "Build workstation")
		}
		if view == "sessions" {
			mustHave(t, body, "Tokens, cost and model need the Claude Code exporter", "Codex session tokens are not collected yet")
		}
	}
	if body := run(claude("installed"), "workers"); !strings.Contains(body, "Claude exporter: installed") || strings.Contains(body, "needs the Minion status-line exporter") {
		t.Fatalf("installed state: %s", body)
	}
	if body := run(claude("installed"), "sessions"); strings.Contains(body, "Tokens, cost and model need the Claude Code exporter") || !strings.Contains(body, "started before the exporter was installed") {
		t.Fatalf("a session on a Worker that has the exporter still asks for the install command: %s", body)
	}
	if body := run(claude("installed"), "accounts"); strings.Contains(body, "No Worker has it yet") {
		t.Fatal("accounts note shown although a Worker has the exporter")
	}
}

func TestUsageSessionsShowAccessKindWithoutHost(t *testing.T) {
	at := time.Now().UnixMilli()
	row := func(name string, custom bool) ops.UsageSessionRow {
		return ops.UsageSessionRow{WorkerID: "build", LastSeen: at, UsageSession: ops.UsageSession{ID: ops.UsageID("fixture", name), Name: name, Harness: "claude", AuthMode: "api", CustomEndpoint: custom, Endpoint: "gateway.example", Source: "session-host", ObservedAt: at}}
	}
	result := ops.UsageListResult{Version: 1, AsOf: at, Sessions: []ops.UsageSessionRow{row("keyed", false), row("gateway", true)}}
	connect(t, usagePageFixtures().reply(ops.OpUsageList, result))
	res := get(t, adminServer(t), "/usage?view=sessions", "X-Test-Session", userHeader)
	body := mainOf(t, res.Body.String())
	mustHave(t, body, "API key", "Custom endpoint")
	if strings.Contains(body, "gateway.example") || strings.Contains(body, "API credential") {
		t.Fatalf("an endpoint host is shown:\n%s", body)
	}
}

func TestUsageShowsReportedUpstreamQuota(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC)
	at := now.UnixMilli()
	weekly := ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: new((7 * 24 * time.Hour).Milliseconds()),
		UsedPercent: new(19.0), ResetsAt: new(now.Add(4 * time.Hour).UnixMilli())}
	account := ops.UsageAccount{ID: ops.UsageID("fixture", "gateway"), Provider: "Anthropic", Label: "Gateway", AuthMode: "api", CustomEndpoint: true,
		Source: "claude-statusline", ObservedAt: at, Windows: []ops.UsageWindow{weekly}}
	unreported := account
	unreported.ID, unreported.Label, unreported.Windows = ops.UsageID("fixture", "unreported"), "No sample", nil
	result := ops.UsageListResult{Version: 1, AsOf: at, Accounts: []ops.UsageAccountRow{
		{UsageAccount: account, WorkerID: "build", Totals: &ops.UsageAccountTotals{Sessions: 1, EstimatedCostUSD: new(0.42)}},
		{UsageAccount: unreported, WorkerID: "build"},
	}, Sessions: []ops.UsageSessionRow{
		{WorkerID: "build", LastSeen: at, UsageSession: ops.UsageSession{ID: ops.UsageID("session", "has-quota"), Name: "Reported gateway", Harness: "claude",
			AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: now.Add(-time.Hour).UnixMilli(), Windows: []ops.UsageWindow{weekly}}},
		{WorkerID: "build", LastSeen: at, UsageSession: ops.UsageSession{ID: ops.UsageID("session", "no-quota"), Name: "Unreported gateway", Harness: "claude",
			AuthMode: "api", CustomEndpoint: true, Source: "session-host", ObservedAt: at}},
	}}
	connect(t, usagePageFixtures().on(ops.OpUsageList, func(args map[string]any) (any, error) {
		if args["subscription_only"] == true {
			return ops.UsageListResult{Version: 1, AsOf: at}, nil
		}
		return result, nil
	}))
	srv := adminServer(t)
	accounts := mainOf(t, get(t, srv, "/usage?view=accounts", "X-Test-Session", userHeader).Body.String())
	// Custom endpoints are one aggregate account: windows stay on the session.
	mustHave(t, accounts, "History · last 90 days · 1 session", "$0.4200")
	mustNotHave(t, accounts, "5-hour limit", "No subscription limits reported.", "Weekly limit", "19.0% / 81.0%")
	sessions := mainOf(t, get(t, srv, "/usage?view=sessions", "X-Test-Session", userHeader).Body.String())
	mustHave(t, sessions, "Upstream quota", "Weekly limit", "19.0% / 81.0%", "Stale window")
	mustNotHave(t, sessions, "5-hour limit", "No subscription limits reported.")
	if strings.Count(sessions, "Weekly limit") != 1 {
		t.Fatal("only the session with a reported window should show it")
	}
	main := mainOf(t, get(t, srv, "/usage?view=overview", "X-Test-Session", userHeader).Body.String())
	mustNotHave(t, main, "Upstream quota", "Weekly limit", "Gateway")
}

func TestUsageSessionsRenderInStoreOrder(t *testing.T) {
	at := time.Now().UnixMilli()
	row := func(name string) ops.UsageSessionRow {
		return ops.UsageSessionRow{WorkerID: "build", LastSeen: at, UsageSession: ops.UsageSession{ID: ops.UsageID("fixture", name), Name: name, AuthMode: "unknown", Source: "session-host", ObservedAt: at}}
	}
	result := ops.UsageListResult{Version: 1, AsOf: at, Sessions: []ops.UsageSessionRow{row("wb-1"), row("wb-2"), row("wb-10")}}
	connect(t, usagePageFixtures().reply(ops.OpUsageList, result))
	body := mainOf(t, get(t, adminServer(t), "/usage?view=sessions", "X-Test-Session", userHeader).Body.String())
	i1, i2, i10 := strings.Index(body, "<h3>wb-1</h3>"), strings.Index(body, "<h3>wb-2</h3>"), strings.Index(body, "<h3>wb-10</h3>")
	if i1 < 0 || i1 >= i2 || i2 >= i10 {
		t.Fatalf("order %d %d %d", i1, i2, i10)
	}
}

func TestUsageShowsLatestResponseTokensSeparately(t *testing.T) {
	at := time.Now().UnixMilli()
	row := ops.UsageSessionRow{WorkerID: "build", LastSeen: at, UsageSession: ops.UsageSession{
		ID: ops.UsageID("session", "claude-response"), Name: "Claude response", Harness: "claude", AuthMode: "api", Source: "claude-statusline", ObservedAt: at,
		UsageCounters: ops.UsageCounters{CacheWriteTokens: new(int64(49167))},
		LastResponseUsage: &ops.UsageResponseTokens{
			InputTokens: new(int64(4)), OutputTokens: new(int64(215)), CacheReadTokens: new(int64(33617)), CacheWriteTokens: new(int64(23308)),
		},
	}}
	connect(t, usagePageFixtures().reply(ops.OpUsageList, ops.UsageListResult{Version: 1, AsOf: at, Sessions: []ops.UsageSessionRow{row}}))
	body := mainOf(t, get(t, adminServer(t), "/usage?view=sessions", "X-Test-Session", userHeader).Body.String())
	mustHave(t, body, "<dt>Input tokens</dt><dd>Unknown</dd>", "<dt>Cache read / write</dt><dd>Unknown / 49167</dd>",
		"<summary>Latest response tokens</summary>", "<dt>Uncached input</dt><dd>4</dd>", "<dt>Output</dt><dd>215</dd>",
		"<dt>Cache read</dt><dd>33617</dd>", "<dt>Cache write</dt><dd>23308</dd>")
	mustNotHave(t, body, "Tokens, cost and model appear once this Claude session reports.")
}

func TestUsageShowsEveryClaudeExporterStateRestartAndStaleReason(t *testing.T) {
	at := time.Now().UnixMilli()
	old := time.Now().Add(-time.Hour).UnixMilli()
	stale := ops.UsageAccountRow{UsageAccount: ops.UsageAccount{ID: ops.UsageID("fixture", "s"), Provider: "Anthropic", Label: "Work", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: old}}
	started := at - 3600000
	live := ops.UsageSessionRow{WorkerID: "build", LastSeen: at, UsageSession: ops.UsageSession{ID: ops.UsageID("fixture", "c"), Name: "c-1", Harness: "claude", AuthMode: "unknown", Source: "session-host", StartedAt: &started, ObservedAt: at}}
	view := func(exporter, section string) string {
		inv := []ops.UsageHarness{{Name: "claude", Status: "installed", Exporter: exporter}}
		result := ops.UsageListResult{Version: 1, AsOf: at, Accounts: []ops.UsageAccountRow{stale}, Sessions: []ops.UsageSessionRow{live},
			Workers: []ops.UsageWorkerRow{{ID: "build", Name: "Build workstation", Online: true, Status: "ready", ObservedAt: &at, InventoryAt: &at, Inventory: inv}}}
		connect(t, usagePageFixtures().reply(ops.OpUsageList, result))
		return mainOf(t, get(t, adminServer(t), "/usage?view="+section, "X-Test-Session", userHeader).Body.String())
	}
	mustHave(t, view("installed", "workers"), "Claude exporter: installed. Restart running Claude sessions to start reporting.")
	mustHave(t, view("overridden", "workers"), "Claude exporter: overridden by managed settings")
	mustHave(t, view("off", "workers"), "Claude exporter: off", "usage_claude_exporter auto")
	mustHave(t, view("conflict", "workers"), "Claude exporter: conflict", "could not be kept")
	mustHave(t, view("installed", "accounts"), "No running Claude Code session of this account has reported since")
}

func TestUsageRestartAdviceOnlyForSilentSessionsStartedBeforeTheExporter(t *testing.T) {
	at := time.Now().UnixMilli()
	before, after := at-3600000, at+1000
	p := usagePage{exporterSeen: map[string]int64{"build": at}}
	row := func(started *int64, model string) ops.UsageSessionRow {
		return ops.UsageSessionRow{WorkerID: "build", UsageSession: ops.UsageSession{Harness: "claude", Model: model, StartedAt: started}}
	}
	cost := 0.5
	reported := row(&before, "")
	reported.EstimatedCostUSD = &cost
	for name, c := range map[string]struct {
		s    ops.UsageSessionRow
		want bool
	}{
		"silent and older":          {row(&before, ""), true},
		"healthy exporter (model)":  {row(&before, "claude-x"), false},
		"healthy exporter (cost)":   {reported, false},
		"started after the install": {row(&after, ""), false},
		"unknown start":             {row(nil, ""), false},
	} {
		if got := p.NeedsRestart(c.s); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if (usagePage{}).NeedsRestart(row(&before, "")) {
		t.Error("advice without an installed exporter")
	}
}
