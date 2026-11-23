package web

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func withRefreshClock(t *testing.T, now *time.Time) {
	t.Helper()
	old := refreshNow
	refreshNow = func() time.Time { return *now }
	workerRefresh.mu.Lock()
	workerRefresh.last = map[string]time.Time{}
	workerRefresh.mu.Unlock()
	t.Cleanup(func() { refreshNow = old })
}

func TestRefreshLimiterAllowsOnePerWorkerPer10Seconds(t *testing.T) {
	now := time.Unix(1000, 0)
	withRefreshClock(t, &now)
	if !workerRefresh.allow("a") || workerRefresh.allow("a") {
		t.Fatal("second refresh within 10 s was allowed")
	}
	if !workerRefresh.allow("b") {
		t.Fatal("another Worker was limited")
	}
	now = now.Add(9 * time.Second)
	if workerRefresh.allow("a") {
		t.Fatal("allowed after 9 s")
	}
	now = now.Add(2 * time.Second)
	if !workerRefresh.allow("a") {
		t.Fatal("not allowed after 11 s")
	}
}

func TestCheckedTimeRendersTimeElement(t *testing.T) {
	ms := int64(1763735400000)
	got := string(checkedTime(&ms, "Never"))
	if !strings.Contains(got, `<time datetime="2025-11-21T14:30:00Z">Nov 21, 2025 14:30 UTC</time>`) {
		t.Fatal(got)
	}
	if string(checkedTime(nil, "Never")) != "Never" {
		t.Fatal("nil must render the fallback")
	}
}

func TestUsageRefreshRateLimitAndErrors(t *testing.T) {
	now := time.Unix(2000, 0)
	withRefreshClock(t, &now)
	has := []string{ops.OpWorkerUsage}
	build := ops.WorkerInfo{ID: "build", Name: "Build box", Online: true, Ops: has}
	bad := ops.WorkerInfo{ID: "bad", Name: "Broken box", Online: true, Ops: has}
	off := ops.WorkerInfo{ID: "off", Name: "Off box", Ops: has}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{build, bad, off}}).
		on(ops.OpWorkerUsage, func(a map[string]any) (any, error) {
			if a["worker_id"] == "bad" {
				return nil, errors.New("timed out")
			}
			return ops.UsageReport{Version: 1}, nil
		})
	connect(t, f)
	srv := onbUser(t)
	form := url.Values{"for_user": {onbUserID}, "view": {"overview"}}
	res := onbPost(t, srv, "/usage/refresh", form)
	body := res.Body.String()
	if !strings.Contains(body, "Could not refresh Broken box") || res.Header().Get("HX-Trigger") != "usage-refreshed" {
		t.Fatalf("%s %v", body, res.Header())
	}
	if n := len(f.called(ops.OpWorkerUsage)); n != 2 {
		t.Fatalf("asked %d Workers, want the 2 online ones", n)
	}
	res = onbPost(t, srv, "/usage/refresh", form)
	if n := len(f.called(ops.OpWorkerUsage)); n != 2 || !strings.Contains(res.Body.String(), "less than 10 seconds ago") {
		t.Fatalf("limit not applied: %d %s", n, res.Body.String())
	}
	now = now.Add(11 * time.Second)
	onbPost(t, srv, "/usage/refresh", form)
	if n := len(f.called(ops.OpWorkerUsage)); n != 4 {
		t.Fatalf("after 11 s asked %d", n)
	}
}

func TestUsagePagesShowLastCheckedAndRefresh(t *testing.T) {
	f := usagePageFixtures()
	connect(t, f)
	srv := adminServer(t)
	for _, view := range []string{"overview", "accounts", "sessions", "workers"} {
		body := mainOf(t, get(t, srv, "/usage?view="+view, "X-Test-Session", adminHeader).Body.String())
		mustHave(t, body, `action="/usage/refresh"`, "Refreshing…", "Last checked", "<time datetime=")
	}
}

func TestWorkerRefreshRoute(t *testing.T) {
	now := time.Unix(3000, 0)
	withRefreshClock(t, &now)
	synced := wAliceInfo
	synced.SessionsSynced = true
	f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{synced}}).
		reply(ops.OpSessionsList, ops.SessionsListResult{})
	connect(t, f)
	s := adminServer(t)
	res := post(t, s, "/workers/w1/refresh", adminHeader, url.Values{})
	if !strings.Contains(res.Body.String(), `id="worker-checked"`) || len(f.called(ops.OpSessionsList)) != 1 {
		t.Fatalf("%s", res.Body.String())
	}
	res = post(t, s, "/workers/w1/refresh", adminHeader, url.Values{})
	if len(f.called(ops.OpSessionsList)) != 1 || !strings.Contains(res.Body.String(), "less than 10 seconds ago") {
		t.Fatalf("limit: %s", res.Body.String())
	}
	now = now.Add(11 * time.Second)
	f.on(ops.OpSessionsList, func(map[string]any) (any, error) { return nil, errors.New("offline") })
	res = post(t, s, "/workers/w1/refresh", adminHeader, url.Values{})
	if !strings.Contains(res.Body.String(), "Could not refresh laptop") {
		t.Fatalf("%s", res.Body.String())
	}
	body := get(t, listServerFor(t), "/workers/w1", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `action="/workers/w1/refresh"`, "Last checked")
}

// A Worker the caller cannot see neither consumes the shared cooldown nor is
// asked; a Worker that answers while the Hub holds no confirmed list is not
// reported as checked (the page would still show the old rows).
func TestWorkerRefreshHonesty(t *testing.T) {
	now := time.Unix(3500, 0)
	withRefreshClock(t, &now)
	f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{wAliceInfo}}).
		reply(ops.OpSessionsList, ops.SessionsListResult{})
	connect(t, f)
	s := adminServer(t)
	res := post(t, s, "/workers/w9/refresh", adminHeader, url.Values{})
	if len(f.called(ops.OpSessionsList)) != 0 || !strings.Contains(res.Body.String(), "not available") {
		t.Fatalf("unknown Worker: %s", res.Body.String())
	}
	if !workerRefresh.allow("w9") {
		t.Fatal("an unknown Worker consumed the cooldown")
	}
	res = post(t, s, "/workers/w1/refresh", adminHeader, url.Values{})
	body := res.Body.String()
	if strings.Contains(body, `id="worker-checked"`) || !strings.Contains(body, "has not received that list") || res.Header().Get("HX-Retarget") != "" {
		t.Fatalf("unsynced Worker reported as checked: %s", body)
	}
}

func listServerFor(t *testing.T) *Server {
	s, _ := workerPageServer(t, snapMine, snapAll, wAliceInfo)
	return s
}

// A Refresh of the Workers view asks for a fresh harness scan, not the
// Worker's cached one, so a change on the Worker shows at once.
func TestUsageRefreshOfWorkersAsksForAFreshInventory(t *testing.T) {
	now := time.Unix(3000, 0)
	withRefreshClock(t, &now)
	has := []string{ops.OpWorkerUsage}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: "w", Name: "Box", Online: true, Ops: has}}}).
		reply(ops.OpUsageList, ops.UsageListResult{Workers: []ops.UsageWorkerRow{{ID: "w", InventoryAt: new(int64(1000))}}}).
		reply(ops.OpWorkerUsage, ops.UsageReport{Version: 1, InventoryAt: new(int64(2000))})
	connect(t, f)
	srv := onbUser(t)
	res := onbPost(t, srv, "/usage/refresh", url.Values{"for_user": {onbUserID}, "view": {"workers"}})
	calls := f.called(ops.OpWorkerUsage)
	if len(calls) != 1 || calls[0].Args["inventory"] != true || calls[0].Args["fresh"] != true || !strings.Contains(res.Body.String(), "Refreshed 1 Worker.") {
		t.Fatalf("worker_usage args %v, want inventory and fresh: %s", calls, res.Body.String())
	}
}

// A cached or older scan (an old Worker ignores the fresh flag) is not
// reported as a refresh.
func TestUsageRefreshOfWorkersDoesNotCountACachedScan(t *testing.T) {
	now := time.Unix(3100, 0)
	withRefreshClock(t, &now)
	has := []string{ops.OpWorkerUsage}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: "w", Name: "Box", Online: true, Ops: has}}}).
		reply(ops.OpUsageList, ops.UsageListResult{Workers: []ops.UsageWorkerRow{{ID: "w", InventoryAt: new(int64(1000))}}}).
		reply(ops.OpWorkerUsage, ops.UsageReport{Version: 1, InventoryAt: new(int64(1000))})
	connect(t, f)
	res := onbPost(t, onbUser(t), "/usage/refresh", url.Values{"for_user": {onbUserID}, "view": {"workers"}})
	body := res.Body.String()
	if strings.Contains(body, "Refreshed 1 Worker") || !strings.Contains(body, "cached or older scan") {
		t.Fatalf("cached scan reported as refreshed: %s", body)
	}
}

// A bulk refresh takes its fan-out slot before the cooldown starts, so two
// requests cannot dispatch one Worker less than refreshEvery apart.
func TestUsageBulkRefreshStartsTheCooldownAtDispatch(t *testing.T) {
	now := time.Unix(3200, 0)
	withRefreshClock(t, &now)
	has := []string{ops.OpWorkerUsage}
	var list []ops.WorkerInfo
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		list = append(list, ops.WorkerInfo{ID: id, Name: id, Online: true, Ops: has})
	}
	release := make(chan struct{})
	f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: list}).
		on(ops.OpWorkerUsage, func(map[string]any) (any, error) { <-release; return ops.UsageReport{Version: 1}, nil })
	connect(t, f)
	srv := onbUser(t)
	done := make(chan struct{})
	go func() {
		onbPost(t, srv, "/usage/refresh", url.Values{"for_user": {onbUserID}, "view": {"overview"}})
		close(done)
	}()
	for len(f.called(ops.OpWorkerUsage)) < 4 { // four slots busy, the fifth waits
		time.Sleep(5 * time.Millisecond)
	}
	if !workerRefresh.allow("e") {
		t.Fatal("the cooldown of a Worker still waiting for a slot was already started")
	}
	close(release)
	<-done
}

// An account card's Refresh runs the native quota query where one exists,
// through the collector, and says what became of it.
func TestAccountRefreshQueuesTheNativeQuotaQuery(t *testing.T) {
	now := time.Unix(3300, 0)
	withRefreshClock(t, &now)
	has := []string{ops.OpWorkerUsage}
	account := ops.UsageID("account", "codex")
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: "w", Name: "Box", Online: true, Ops: has}}}).
		reply(ops.OpWorkerUsage, ops.UsageReport{Version: 1}).
		reply(ops.OpUsageCollectorSet, ops.UsageCollector{Mode: "native_background", CanStart: true})
	connect(t, f)
	srv := onbUser(t)
	form := url.Values{"for_user": {onbUserID}, "view": {"accounts"}, "worker": {"w"}, "account": {account}, "native": {"1"}, "source": {"w|"}}
	res := onbPost(t, srv, "/usage/refresh", form)
	calls := f.called(ops.OpUsageCollectorSet)
	if len(calls) != 1 || calls[0].Args["start"] != true || calls[0].Args["account_id"] != account || calls[0].Args["worker_id"] != "w" || !strings.Contains(res.Body.String(), "quota query is queued") {
		t.Fatalf("collector %v: %s", calls, res.Body.String())
	}
	// Rate limited: the collector's next due time is in the future.
	later := now.Add(time.Hour).UnixMilli()
	f.reply(ops.OpUsageCollectorSet, ops.UsageCollector{Mode: "native_background", CanStart: true, NextDue: &later})
	now = now.Add(11 * time.Second)
	res = onbPost(t, srv, "/usage/refresh", form)
	if !strings.Contains(res.Body.String(), "rate-limited until") {
		t.Fatalf("%s", res.Body.String())
	}
	// No native query for this account: no collector call, and the text says why.
	form.Del("native")
	now = now.Add(11 * time.Second)
	res = onbPost(t, srv, "/usage/refresh", form)
	if len(f.called(ops.OpUsageCollectorSet)) != 2 || !strings.Contains(res.Body.String(), "no background query") {
		t.Fatalf("%s", res.Body.String())
	}
}

// "Last checked" of a Usage page is the newest observation on display, never
// the moment the Hub read its store.
func TestUsageLastCheckedIsTheDataTimeNotThePageRead(t *testing.T) {
	var p usagePage
	p.Rows.AsOf = 9000
	if p.LastChecked() != nil || string(p.Checked(p.LastChecked())) != "Never" {
		t.Fatal("an empty scope must show Never, not the read time")
	}
	inventory := int64(4000)
	p.Rows.Workers = []ops.UsageWorkerRow{{ID: "w", InventoryAt: &inventory}}
	p.Rows.Sessions = []ops.UsageSessionRow{{UsageSession: ops.UsageSession{ObservedAt: 3000}, LastSeen: 5000}}
	if got := p.LastChecked(); got == nil || *got != 5000 {
		t.Fatalf("LastChecked = %v, want 5000", got)
	}
}
