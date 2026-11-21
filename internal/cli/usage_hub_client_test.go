package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageCLIInventoryAndOverviewUseConfiguredHub(t *testing.T) {
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, _ *hubStub) {
		switch r.URL.Path {
		case ops.RESTPrefix + ops.OpWorkerUsage:
			stubJSON(w, http.StatusOK, `{"version":1,"inventory":[{"name":"codex","version":"0.171.0","status":"installed"}]}`)
		case ops.RESTPrefix + ops.OpUsageList:
			stubJSON(w, http.StatusOK, `{"version":1,"as_of":1760000000000,"overview":{"accounts":0,"without_windows":0,"groups":[]}}`)
		default:
			stubJSON(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"unexpected operation"}}`)
		}
	})
	local := localAnswers(`{}`)
	deps := depsWithSettings(local, hub.env())
	code, out, errs := runWith(t, deps, "", "worker_usage", "--worker-id", hubWorkerID, "--inventory", "--json")
	if code != 0 || !strings.Contains(out, "0.171.0") || strings.Contains(out+errs, hub.key) {
		t.Fatalf("inventory exit %d, stdout %q, stderr %q", code, out, errs)
	}
	code, out, errs = runWith(t, deps, "", "usage_list", "--section", "overview", "--json")
	if code != 0 || !strings.Contains(out, "without_windows") || strings.Contains(out+errs, hub.key) {
		t.Fatalf("overview exit %d, stdout %q, stderr %q", code, out, errs)
	}
	requests := hub.requests()
	if len(requests) != 2 || requests[0].Body["worker_id"] != hubWorkerID || requests[0].Body["inventory"] != true ||
		requests[1].Body["section"] != "overview" || local.count() != 0 {
		t.Fatalf("unexpected inventory/overview routing: %d Hub requests, %d local requests", len(requests), local.count())
	}
}
