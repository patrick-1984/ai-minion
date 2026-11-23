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
	f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{wAliceInfo}}).
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
		reply(ops.OpWorkerUsage, ops.UsageReport{Version: 1})
	connect(t, f)
	srv := onbUser(t)
	onbPost(t, srv, "/usage/refresh", url.Values{"for_user": {onbUserID}, "view": {"workers"}})
	calls := f.called(ops.OpWorkerUsage)
	if len(calls) != 1 || calls[0].Args["inventory"] != true || calls[0].Args["fresh"] != true {
		t.Fatalf("worker_usage args %v, want inventory and fresh", calls)
	}
}
