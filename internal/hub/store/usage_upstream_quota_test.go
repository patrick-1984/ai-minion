package store

import (
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageKeepsLastReportedUpstreamQuota(t *testing.T) {
	s := openTest(t)
	workers, _, now := usageFixture(t, s, 0)
	at := now.UnixMilli()
	reportAt := now.Add(10 * time.Second)
	account := ops.UsageAccount{ID: ops.UsageID("account", "custom", "Anthropic"), Provider: "Anthropic", AuthMode: "api", CustomEndpoint: true, Source: "session-host"}
	save := func(worker int, source string, delay time.Duration, windows ...ops.UsageWindow) {
		t.Helper()
		observed := account
		observed.ObservedAt, observed.Source, observed.Windows = at+delay.Milliseconds(), source, windows
		if source == "claude-statusline" {
			observed.SourceSessionID = ops.UsageID("session", workers[worker].ID)
		}
		reportAt = reportAt.Add(time.Millisecond)
		report := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: reportAt.UnixMilli(), Accounts: []ops.UsageAccount{observed}}
		if err := s.SaveUsage(t.Context(), workers[worker].ID, workers[worker].OwnerID, report, reportAt); err != nil {
			t.Fatal(err)
		}
	}
	read := func(scope []ops.WorkerInfo, worker string, delay time.Duration, want ...string) {
		t.Helper()
		got, err := s.ReadUsage(t.Context(), scope, "alice", ops.UsageListArgs{Section: "accounts"}, now.Add(time.Minute))
		if err != nil || len(got.Accounts) != 1 {
			t.Fatalf("accounts: %+v, %v", got.Accounts, err)
		}
		a := got.Accounts[0]
		if a.WorkerID != worker || a.ObservedAt != at+delay.Milliseconds() || len(a.Windows) != len(want) {
			t.Fatalf("account sample: %+v, want Worker %s with %v", a, worker, want)
		}
		for i, scope := range want {
			if a.Windows[i].Scope != scope {
				t.Errorf("window %d: got %q, want %q", i, a.Windows[i].Scope, scope)
			}
		}
	}
	short := ops.UsageWindow{Scope: "short", Period: "rolling", UsedPercent: new(18.0)}
	weekly := ops.UsageWindow{Scope: "weekly", Period: "rolling", UsedPercent: new(19.0)}
	// Custom endpoints are not told apart: upstream windows stay on the
	// session and never attach to the aggregate API account.
	save(0, "session-host", 2*time.Second)
	save(0, "claude-statusline", time.Second, short, weekly)
	save(1, "claude-statusline", 6*time.Second, weekly)
	got0, err := s.ReadUsage(t.Context(), workers, "alice", ops.UsageListArgs{Section: "accounts"}, now.Add(time.Minute))
	if err != nil || len(got0.Accounts) != 1 || len(got0.Accounts[0].Windows) != 0 {
		t.Fatalf("API account carries windows: %+v, %v", got0.Accounts, err)
	}
	var stored int
	if err := s.db.QueryRow(`SELECT count(*) FROM usage_accounts WHERE json_array_length(body,'$.windows')>0`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("stored API windows: %d, %v", stored, err)
	}
	_ = read
	got, err := s.ReadUsage(t.Context(), workers, "alice", ops.UsageListArgs{Section: "overview"}, now.Add(time.Minute))
	if err != nil || got.Overview == nil || got.Overview.Accounts != 0 {
		t.Fatalf("custom endpoint entered the subscription overview: %+v, %v", got.Overview, err)
	}
	var collectors int
	if err := s.db.QueryRow(`SELECT count(*) FROM usage_collectors`).Scan(&collectors); err != nil || collectors != 0 {
		t.Fatalf("custom endpoint entered subscription collectors: %d, %v", collectors, err)
	}
}

// A session bucket keeps its own totals, and no account card carries upstream
// windows of a custom endpoint.
func TestUsageQuotaStaysWithItsOwnEndpointAccount(t *testing.T) {
	s := openTest(t)
	workers, _, now := usageFixture(t, s, 0)
	at := now.UnixMilli()
	idA := ops.UsageID("account", "custom", "Anthropic", "endpoint-a")
	idB := ops.UsageID("account", "custom", "Anthropic", "endpoint-b")
	account := func(id string, delay int64, used float64) ops.UsageAccount {
		return ops.UsageAccount{ID: id, Provider: "Anthropic", AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at + delay,
			SourceSessionID: ops.UsageID("session", id), Windows: []ops.UsageWindow{{Scope: "short", Period: "rolling", UsedPercent: &used}}}
	}
	count := int64(7)
	sessionA := ops.UsageSession{ID: ops.UsageID("session", "a"), Name: "a", AuthMode: "api", CustomEndpoint: true, AccountID: idA, ObservedAt: at, Source: "claude-statusline", UsageCounters: ops.UsageCounters{InputTokens: &count}}
	sessionB := sessionA
	sessionB.ID, sessionB.Name, sessionB.AccountID = ops.UsageID("session", "b"), "b", idB
	report := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: at + 10000, Complete: true, Sessions: []ops.UsageSession{sessionA, sessionB},
		Accounts: []ops.UsageAccount{account(idA, 0, 11), account(idB, 5000, 99)}}
	if err := s.SaveUsage(t.Context(), "w1", "alice", report, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	bucket, err := s.SetUsageBucket(t.Context(), "alice", workers[:1], ops.UsageBucketSetArgs{Action: "save", Kind: "session", Label: "Only A"})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := s.ReadUsage(t.Context(), workers[:1], "alice", ops.UsageListArgs{Section: "sessions"}, now)
	if err != nil || len(sessions.Sessions) != 2 {
		t.Fatalf("sessions: %+v, %v", sessions.Sessions, err)
	}
	if _, err := s.SetUsageBucket(t.Context(), "alice", workers[:1], ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: bucket.ID, Members: []string{sessionOf(sessions.Sessions, "a")}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadUsage(t.Context(), workers[:1], "alice", ops.UsageListArgs{Section: "accounts", BucketID: bucket.ID}, now.Add(time.Minute))
	if err != nil || len(got.Accounts) != 1 {
		t.Fatalf("accounts in the bucket: %+v, %v", got.Accounts, err)
	}
	a := got.Accounts[0]
	if a.ID != idA || len(a.Windows) != 0 || a.Totals == nil || a.Totals.Sessions != 1 {
		t.Fatalf("bucket card: %+v", a)
	}
	all, err := s.ReadUsage(t.Context(), workers[:1], "alice", ops.UsageListArgs{Section: "accounts"}, now.Add(time.Minute))
	if err != nil || len(all.Accounts) != 2 {
		t.Fatalf("accounts: %+v, %v", all.Accounts, err)
	}
	for _, c := range all.Accounts {
		if len(c.Windows) != 0 {
			t.Errorf("account %s carries windows %+v", c.ID, c.Windows)
		}
	}
}

func sessionOf(rows []ops.UsageSessionRow, name string) string {
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	return ""
}
