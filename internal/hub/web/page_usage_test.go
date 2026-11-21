package web

import (
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func usagePageFixtures() *fakeOps {
	now := time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC)
	at := now.UnixMilli()
	duration := int64(5 * time.Hour / time.Millisecond)
	reset := now.Add(3 * time.Hour).UnixMilli()
	percent := 47.5
	tokens := int64(8100)
	account := ops.UsageAccountRow{UsageAccount: ops.UsageAccount{ID: ops.UsageID("account", "example"), Provider: "OpenAI", Label: "Personal subscription", AuthMode: "subscription", Source: "codex-app-server", ObservedAt: at, Windows: []ops.UsageWindow{{Scope: "Codex", Period: "rolling", DurationMS: &duration, ResetsAt: &reset, UsedPercent: &percent}}}, WorkerID: "build"}
	out := ops.UsageListResult{Version: 1, AsOf: at, Workers: []ops.UsageWorkerRow{{ID: "build", Name: "Build workstation", ObservedAt: &at, Inventory: []ops.UsageHarness{{Name: "Claude Code", Version: "2.1.80", Status: "installed"}, {Name: "Gemini CLI", Status: "unavailable"}}}, {ID: "laptop", Name: "Travel laptop (offline)"}}, Sessions: []ops.UsageSessionRow{{UsageSession: ops.UsageSession{ID: ops.UsageID("session", "example"), Name: "release-validation-on-build-workstation", Folder: "/workspace/synthetic/release-validation", Harness: "Codex", Version: "0.126.0", Model: "test-model", AuthMode: "subscription", Source: "native-event", StartedAt: &at, UsageCounters: ops.UsageCounters{InputTokens: &tokens}}}}, Accounts: []ops.UsageAccountRow{account}}
	overview := ops.SummarizeUsage(out.Accounts, now)
	out.Overview = &overview
	return newFakeOps().reply(ops.OpUsageList, out).reply(ops.OpUsageBucketList, ops.UsageBucketListResult{Buckets: []ops.UsageBucket{{ID: "a", Kind: "session", Label: "Release validation"}, {ID: "b", Kind: "provider", Label: "Personal accounts"}}})
}

func TestUsageViewsShowDisclosedFixtures(t *testing.T) {
	connect(t, usagePageFixtures())
	srv := adminServer(t)
	for _, test := range []struct{ view, want string }{{"overview", "47.5%"}, {"accounts", "Using faster"}, {"sessions", "release-validation-on-build-workstation"}, {"workers", "Scan installed harnesses"}, {"buckets", "Release validation"}} {
		for _, role := range []string{adminHeader, userHeader} {
			res := get(t, srv, "/usage?view="+test.view, "X-Test-Session", role)
			if res.Code != 200 {
				t.Fatalf("%s returned %d", test.view, res.Code)
			}
			mustHave(t, res.Body.String(), test.want, "Monitoring and access", "including unshared sessions")
		}
	}
}
