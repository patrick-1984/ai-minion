package web

import (
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func harnessFixtures(wanted []string) *fakeOps {
	at := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC).UnixMilli()
	usageOp := []string{ops.OpWorkerUsage}
	workers := []ops.WorkerInfo{
		{ID: "build", Name: "Build box", Owner: "alice", Online: true, Ops: usageOp},
		{ID: "full", Name: "Full box", Owner: "alice", Online: true, Ops: usageOp},
		{ID: "laptop", Name: "Travel laptop", Owner: "alice"},
		{ID: "fresh", Name: "Fresh box", Owner: "alice", Online: true, Ops: usageOp},
		{ID: "pending", Name: "Pending", Pending: true},
	}
	rows := []ops.UsageWorkerRow{
		{ID: "build", InventoryAt: &at, Online: true, Inventory: []ops.UsageHarness{{Name: "claude", Version: "2.1.80", Status: "installed"}, {Name: "codex", Status: "unavailable"}}},
		{ID: "full", InventoryAt: &at, Online: true, Inventory: []ops.UsageHarness{{Name: "claude", Status: "installed"}, {Name: "codex", Version: "0.126.0", Status: "installed"}}},
	}
	return newFakeOps().
		reply(ops.OpAccountPrefs, ops.AccountPrefsResult{WantedHarnesses: wanted}).
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: workers}).
		reply(ops.OpUsageList, ops.UsageListResult{Workers: rows}).
		reply(ops.OpAccountPrefsSet, delivered).
		reply(ops.OpWorkerUsage, ops.UsageReport{Version: 1})
}

func TestHarnessesMatrixStates(t *testing.T) {
	f := harnessFixtures([]string{"claude", "codex"})
	connect(t, f)
	srv := onbUser(t)
	body := mainOf(t, onbGet(t, srv, "/harnesses").Body.String())
	mustHave(t, body, "Harnesses I want", "Anthropic", "Claude Code", "Google", "Alibaba", "Open source", "OpenCode",
		"DeepSeek has no official CLI", "Check all my Workers", "Installed v2.1.80", "Installed v0.126.0", "Missing",
		"Unknown (offline, not scanned)", "Unknown (not scanned)", "Nov 21, 2026 14:30 UTC", `hx-trigger="every 30s"`, `href="/usage?view=workers"`)
	mustHave(t, body, `name="harness" value="claude" checked`, `name="harness" value="codex" checked`)
	mustNotHave(t, body, `value="gemini" checked`, "Pending", `id="scope-switch"`)
	// Only Workers missing something.
	body = mainOf(t, onbGet(t, srv, "/harnesses?missing=1").Body.String())
	mustHave(t, body, "Build box")
	mustNotHave(t, body, "Full box", "Travel laptop")
	// Never chosen: every known harness is offered and compared.
	f = harnessFixtures(nil)
	connect(t, f)
	body = mainOf(t, onbGet(t, onbUser(t), "/harnesses").Body.String())
	mustHave(t, body, `name="harness" value="gemini" checked`, `name="harness" value="cline" checked`)
}

func TestHarnessesAdminScopeToggle(t *testing.T) {
	f := harnessFixtures(nil)
	connect(t, f)
	srv := onbAdmin(t)
	body := mainOf(t, onbGet(t, srv, "/harnesses").Body.String())
	mustHave(t, body, `id="scope-switch"`, `href="/harnesses?scope=all"`)
	if c := f.called(ops.OpWorkersList); len(c) != 1 || c[0].Args["all"] == true {
		t.Fatalf("default scope must be the admin's own Workers: %+v", c)
	}
	body = mainOf(t, onbGet(t, srv, "/harnesses?scope=all").Body.String())
	mustHave(t, body, `hx-get="/harnesses?scope=all"`, "(alice)")
	if c := f.called(ops.OpWorkersList); len(c) != 2 || c[1].Args["all"] != true {
		t.Fatalf("All users did not widen the list: %+v", c)
	}
}

func TestHarnessesSaveChoiceAndScan(t *testing.T) {
	f := harnessFixtures(nil)
	connect(t, f)
	srv := onbUser(t)
	res := onbPost(t, srv, "/harnesses/choose", url.Values{"for_user": {onbUserID}, "harness": {"claude", "aider", "bogus"}})
	if res.Code != 200 || res.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("choose = %d %s", res.Code, res.Body.String())
	}
	set := f.called(ops.OpAccountPrefsSet)
	if len(set) != 1 {
		t.Fatalf("calls: %+v", set)
	}
	if got, _ := set[0].Args["wanted_harnesses"].([]any); len(got) != 2 || got[0] != "claude" || got[1] != "aider" {
		t.Errorf("stored %+v (unknown names must be dropped)", set[0].Args)
	}
	if res := onbPost(t, srv, "/harnesses/choose", url.Values{"for_user": {"someone-else"}}); !strings.Contains(res.Body.String(), "account changed") || len(f.called(ops.OpAccountPrefsSet)) != 1 {
		t.Errorf("a stale form was accepted: %s", res.Body.String())
	}
	res = onbPost(t, srv, "/harnesses/scan", url.Values{"for_user": {onbUserID}})
	if res.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("scan = %d %s", res.Code, res.Body.String())
	}
	scans := f.called(ops.OpWorkerUsage)
	if len(scans) != 3 {
		t.Fatalf("expected a scan of the 3 online Workers (not the offline one or the pending one): %+v", scans)
	}
	for _, c := range scans {
		if c.Args["inventory"] != true || c.Args["fresh"] != true {
			t.Errorf("scan did not request fresh inventory: %+v", c.Args)
		}
	}
}

func TestHarnessCheckAllCountsOnlyWorkersWhoseInventoryAdvanced(t *testing.T) {
	at := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC).UnixMilli()
	newer := at + 1000
	var mu sync.Mutex
	stored := map[string]int64{"build": at, "full": at}
	f := harnessFixtures([]string{"claude"}).on(ops.OpWorkerUsage, func(args map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args["worker_id"] {
		case "build":
			stored["build"] = newer // the Hub recorded this scan
			return ops.UsageReport{Version: 1, InventoryAt: &newer}, nil
		case "full":
			return ops.UsageReport{Version: 1, InventoryAt: &at}, nil // cached
		case "fresh":
			// A newer background sample committed first: the Hub discarded
			// this scan although the reply looks newer.
			return ops.UsageReport{Version: 1, InventoryAt: &newer}, nil
		}
		return ops.UsageReport{Version: 1}, nil // no inventory
	}).on(ops.OpUsageList, func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		var rows []ops.UsageWorkerRow
		for _, id := range []string{"build", "full"} {
			v := stored[id]
			rows = append(rows, ops.UsageWorkerRow{ID: id, InventoryAt: &v, Online: true})
		}
		return ops.UsageListResult{Workers: rows}, nil
	})
	connect(t, f)
	srv := onbUser(t)
	res := onbPost(t, srv, "/harnesses/scan", url.Values{"for_user": {onbUserID}})
	fl := flashesOf(res)
	if len(fl) != 1 || !strings.Contains(fl[0].Text, "Checked 1 Worker.") || !strings.Contains(fl[0].Text, "2 Workers could not be checked") {
		t.Fatalf("flash: %+v", fl)
	}
}
