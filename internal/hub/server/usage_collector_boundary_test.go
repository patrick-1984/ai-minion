package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageHTTPStartRespectsNativeAttestationAndCooldown(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	root, _ := d.login("root")
	owner := d.userID("root")
	worker := d.enrol("collector-fixture", owner, store.WorkerKindPrivate)
	now := time.Now()
	at := now.UnixMilli()
	codex := ops.UsageID("fixture-account", "codex")
	claude := ops.UsageID("fixture-account", "claude")
	incompatible := ops.UsageID("fixture-account", "wrong provider")

	fixtures := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: at, Accounts: []ops.UsageAccount{
		{ID: codex, Provider: "OpenAI", AuthMode: "subscription", Source: "codex-account", ObservedAt: at},
		{ID: claude, Provider: "Anthropic", AuthMode: "subscription", Source: "claude-statusline", ObservedAt: at},
		{ID: incompatible, Provider: "Anthropic", AuthMode: "subscription", Source: "codex-account", ObservedAt: at},
	}}
	if err := d.st.SaveUsage(t.Context(), worker, owner, fixtures, now); err != nil {
		t.Fatal(err)
	}
	start := func(account string, want int) ops.UsageCollector {
		t.Helper()
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+ops.OpUsageCollectorSet,
			ops.UsageCollectorSetArgs{AccountID: account, WorkerID: worker, Start: true}, root)
		var got ops.UsageCollector
		if code != want {
			t.Errorf("Start collector status returned %d, want %d: %s", code, want, body)
		} else if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
		}
		return got
	}
	start(claude, http.StatusConflict)
	start(incompatible, http.StatusConflict)
	first := start(codex, http.StatusOK)
	if !first.CanStart || first.Mode != "native_background" || first.PreferredWorker != worker || first.Status == "" {
		t.Errorf("Start response omitted its live collector state: %+v", first)
	}
	lease, err := d.st.ClaimUsagePoll(t.Context(), worker, owner, now)
	if err != nil || lease.AccountID != codex {
		t.Fatalf("native queue selected a different account: %+v, %v", lease, err)
	}
	start(codex, http.StatusOK)
	cooldown := 49 * time.Minute
	if err := d.st.CompleteUsagePoll(t.Context(), lease, nil, "rate_limited", cooldown, now); err != nil {
		t.Errorf("repeated Start invalidated the running native query: %v", err)
	}
	queued := start(codex, http.StatusOK)
	if queued.Status != "rate_limited" || queued.NextDue == nil || *queued.NextDue < now.Add(cooldown).UnixMilli() {
		t.Errorf("queued Start lost throttled state or Retry-After: %+v", queued)
	}
	later, err := d.st.ClaimUsagePoll(t.Context(), worker, owner, now.Add(30*time.Minute))
	if err != nil || later.ID != "" {
		t.Errorf("queued Start bypassed Retry-After: %+v, %v", later, err)
	}
}

func TestUsageHTTPCollectorChangeCannotRevealReassignedWorker(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	alice := d.user("alice")
	bob := d.user("bob")
	charlie := d.user("charlie")
	headers, _ := d.login("alice")
	remaining := d.enrol("alice-fixture", alice, store.WorkerKindPrivate)
	former := d.enrol("bob-collector-fixture", bob, store.WorkerKindInfrastructure)
	now := time.Now()
	account := ops.UsageAccount{ID: ops.UsageID("fixture-account", "shared"), Provider: "OpenAI",
		AuthMode: "subscription", Source: "codex-account", ObservedAt: now.UnixMilli()}
	report := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: now.UnixMilli(), Accounts: []ops.UsageAccount{account}}
	for _, c := range []struct{ worker, owner string }{{remaining, alice}, {former, bob}} {
		if err := d.st.SaveUsage(t.Context(), c.worker, c.owner, report, now); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := d.st.ClaimUsagePoll(t.Context(), former, bob, now)
	if err != nil || lease.ID == "" {
		t.Fatalf("previous collector never acquired native claim: %+v, %v", lease, err)
	}
	novel := now.Add(time.Second)
	account.ObservedAt = novel.UnixMilli()
	account.Source = "codex-app-server"
	if err := d.st.CompleteUsagePoll(t.Context(), lease, &account, "ready", 0, novel); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.ReassignWorker(t.Context(), store.ReassignParams{WorkerID: former, NewOwnerID: charlie, AtMS: novel.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+ops.OpUsageCollectorSet, ops.UsageCollectorSetArgs{AccountID: account.ID, WorkerID: remaining, Start: true}, headers)
	var got ops.UsageCollector
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("remaining eligible owner could not select their own collector: %d %s", code, body)
	}
	if got.ActiveWorker != "" || got.LastSuccess != nil || got.PreferredWorker != remaining {
		t.Errorf("write response disclosed the reassigned Worker's last observation: %+v", got)
	}
	if got.NextDue != nil {
		t.Errorf("write response exposed the previous schedule: %+v", got)
	}
	early, err := d.st.ClaimUsagePoll(t.Context(), remaining, alice, now.Add(time.Minute))
	if err != nil || early.ID != "" {
		t.Errorf("reassignment erased the account's existing cooldown: %+v, %v", early, err)
	}
	next, err := d.st.ClaimUsagePoll(t.Context(), remaining, alice, now.Add(6*time.Minute))
	if err != nil || next.AccountID != account.ID {
		t.Errorf("remaining collector did not resume after cooldown: %+v, %v", next, err)
	}
}
