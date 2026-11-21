package cli

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
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

func TestUsageCLILocalReviewAndAcceptance(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	enrollment := store.Enrollment{HubPub: hub.Public(), WorkerID: hubWorkerID, Owner: "fixture-owner"}
	if err := st.SaveEnrollment(ctx, enrollment); err != nil {
		t.Fatal(err)
	}
	checkGrant := func(want bool) {
		t.Helper()
		allowed, err := st.UsageConnectionAllowed(ctx, enrollment.WorkerID)
		if err != nil || allowed != want {
			t.Fatalf("connection admission = %t, %v, want %t", allowed, err, want)
		}
		granted, err := st.UsageRegistration(ctx, enrollment.WorkerID)
		if err != nil || granted != want {
			t.Fatalf("monitoring grant = %t, %v, want %t", granted, err, want)
		}
	}
	checkGrant(false)
	out, _, err := configCmd(t, testConfigDeps(nil), dir, "usage")
	if err != nil || !strings.Contains(out, ops.UsageDisclosure) || !strings.Contains(out, hub.Fingerprint().String()) ||
		!strings.Contains(out, enrollment.WorkerID) || !strings.Contains(out, enrollment.Owner) ||
		!strings.Contains(out, "minion worker config usage --accept") {
		t.Fatalf("local review: %q, %v", out, err)
	}
	checkGrant(false)
	out, _, err = configCmd(t, testConfigDeps(nil), dir, "usage", "--accept")
	if err != nil || !strings.Contains(out, "Usage monitoring accepted for this enrollment.") {
		t.Fatalf("local acceptance: %q, %v", out, err)
	}
	checkGrant(true)

	enrollment.Owner = "different-fixture-owner"
	if err := st.SaveEnrollment(ctx, enrollment); err != nil {
		t.Fatal(err)
	}
	checkGrant(false)
}
