// Package usagecontract checks the contract between the Hub and the Worker on
// the usage-meters document. The two meet only on the wire, so the check lives
// outside both trees.
package usagecontract

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
)

func rbAcct(id, provider string, observed time.Time, ws ...ops.UsageWindow) ops.UsageAccount {
	return ops.UsageAccount{ID: ops.UsageID("account", id), Provider: provider, AuthMode: "subscription", ObservedAt: observed.UnixMilli(), Windows: ws}
}

// Hub-built documents always pass the Worker's validator, also with edge
// numbers: overflow-prone amounts and token counters.
func TestHubDocumentsPassTheWorkerValidatorWithEdgeNumbers(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UnixMilli()
	huge := math.MaxFloat64 / 2
	big := int64(math.MaxInt64)
	var in, out = &big, &big
	accts := []ops.UsageAccount{
		rbAcct("a", "google", now, ops.UsageWindow{Scope: "s", Period: "fixed", Unit: "tokens", Used: fptr(huge), Remaining: fptr(1e300), Limit: fptr(huge), ResetsAt: &reset}),
		rbAcct("b", "google", now, ops.UsageWindow{Scope: "s", Period: "fixed", Unit: "tokens", Used: fptr(huge), Remaining: fptr(1e300), Limit: fptr(huge), ResetsAt: &reset}),
		rbAcct("c", "google", now, ops.UsageWindow{Scope: "t", Period: "fixed", Unit: "usd", Used: fptr(math.MaxFloat64), ResetsAt: &reset}),
		rbAcct("d", "anthropic", now, ops.UsageWindow{Scope: "", Period: "rolling", DurationMS: iptr(math.MaxInt64), ResetsAt: &reset, UsedPercent: fptr(42)}),
	}
	sess := []ops.UsageSessionObservation{
		{UsageSession: ops.UsageSession{ID: "s1", ObservedAt: now.UnixMilli(), UsageCounters: ops.UsageCounters{InputTokens: in, OutputTokens: out, CostUSD: fptr(math.MaxFloat64)}}},
		{UsageSession: ops.UsageSession{ID: "s2", ObservedAt: now.UnixMilli(), UsageCounters: ops.UsageCounters{InputTokens: in, OutputTokens: out, CostUSD: fptr(math.MaxFloat64)}}},
	}
	pm, _ := ops.ProviderMeters(accts, now)
	sm, _ := ops.SessionMeters(sess, accts, now)
	doc := ops.UsageMeters{Schema: ops.UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []ops.UsageSlotDoc{
		ops.NewUsageSlot("user", "provider", "P", pm, 0, now),
		ops.NewUsageSlot("domain", "session", "S", sm, 0, now),
	}}
	if err := doc.Fit(ops.UsageMetersMaxBytes); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := presence.CheckUsage(b); err != nil {
		t.Fatalf("the Worker rejects a Hub document: %v\n%s", err, b)
	}
	// The tokens meter is the exact sum, not a wrapped counter.
	for _, m := range sm {
		if m.Metric == "sessions.tokens" && *m.Used < 0 {
			t.Fatalf("wrapped sum %v", *m.Used)
		}
	}
}

func fptr(v float64) *float64 { return &v }
func iptr(v int64) *int64     { return &v }
