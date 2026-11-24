package web

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestSinkTextShowsLastSuccessErrorAndDropReasons(t *testing.T) {
	ts := ops.Timestamp(time.Date(2026, 11, 23, 23, 29, 0, 0, time.UTC))
	got := sinkText(ops.LogSinkStatus{Name: "otel", Enabled: true, Destination: "https://x", Level: "info", Generation: 4,
		LastSuccess: &ts, LastError: "export failed: connection refused", Dropped: map[string]uint64{"export_failed": 78, "reconfigure": 3}})
	for _, w := range []string{"last success: 2026-11-23 23:29 UTC", "last error: export failed: connection refused", "export_failed 78, reconfigure 3"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
	if g := sinkText(ops.LogSinkStatus{Name: "syslog", Enabled: true, Destination: "udp://h", Level: "info"}); !strings.Contains(g, "last success: never") {
		t.Errorf("got %q", g)
	}
}

func TestTrustedProxyHint(t *testing.T) {
	q := ops.HubDiagnosticsRequest{PeerAddress: "10.0.2.9", ForwardedHeaders: []ops.HubDiagnosticsHeader{{Name: "X-Forwarded-For", Value: "10.3.3.11"}}}
	h := trustedProxyHint(q)
	for _, w := range []string{"10.0.2.9/32", "MINION_TRUSTED_PROXIES", "only your proxy can reach"} {
		if !strings.Contains(h, w) {
			t.Errorf("missing %q in %q", w, h)
		}
	}
	if strings.Contains(h, "/24") {
		t.Errorf("hint suggests a guessed network: %q", h)
	}
	q.PeerTrustedProxy = true
	if trustedProxyHint(q) != "" {
		t.Error("hint for a trusted peer")
	}
	if trustedProxyHint(ops.HubDiagnosticsRequest{PeerAddress: "10.0.2.9"}) != "" {
		t.Error("hint without forwarding headers")
	}
}

func TestSessionUsageFacts(t *testing.T) {
	in := int64(12)
	created := time.Unix(1700000000, 0)
	start, earlier, stopped := created.UnixMilli(), created.Add(-time.Hour).UnixMilli(), int64(1700000500000)
	rows := []ops.UsageSessionRow{{UsageSession: ops.UsageSession{Name: "other", Harness: "codex", StartedAt: &start}},
		// An earlier run under the same name, newer report: never shown.
		{UsageSession: ops.UsageSession{Name: "mt", Harness: "codex", Model: "old-model", StartedAt: &earlier, ObservedAt: 1800000000000}},
		{LastSeen: 1700000001000, UsageSession: ops.UsageSession{Name: "mt", Harness: "claude", Version: "2.1.3", Model: "haiku", AuthMode: "api_key", ObservedAt: 1700000000000, StartedAt: &start,
			UsageCounters: ops.UsageCounters{InputTokens: &in}}}}
	got := sessionUsageFacts(rows, "mt", created)
	text := ""
	for _, f := range got {
		text += f.Label + "=" + f.Value + ";"
	}
	for _, w := range []string{"Harness=claude 2.1.3", "Model=haiku", "Access=api_key", "Input tokens=12"} {
		if !strings.Contains(text, w) {
			t.Errorf("missing %q in %s", w, text)
		}
	}
	if strings.Contains(text, "Output tokens") || strings.Contains(text, "cost") || strings.Contains(text, "old-model") {
		t.Errorf("unknown or stale values shown: %s", text)
	}
	if sessionUsageFacts(rows, "none", created) != nil {
		t.Error("facts for an unknown session")
	}
	if sessionUsageFacts(rows, "mt", time.Time{}) != nil {
		t.Error("facts without a live identity")
	}
	// Last listed within its creation second: it may be an earlier run
	// recreated in that same second, so nothing is shown.
	rows[2].LastSeen = 1700000000999
	if sessionUsageFacts(rows, "mt", created) != nil {
		t.Error("facts of a row last listed within the creation second")
	}
	rows[2].LastSeen = 1700000001000
	// Two live rows of that second: ambiguous, nothing is shown.
	twin := rows[2]
	twin.Model = "other-run"
	if sessionUsageFacts(append(slices.Clone(rows), twin), "mt", created) != nil {
		t.Error("facts from an ambiguous match")
	}
	// The live run ended: its row is not the live one.
	rows[2].StoppedAt = &stopped
	if sessionUsageFacts(rows, "mt", created) != nil {
		t.Error("facts of an ended run")
	}
}

func TestSessionCountersHonestZeroCostAndDurationSource(t *testing.T) {
	zero := 0.0
	ms := int64(107 * 3600 * 1000)
	row := ops.UsageSessionRow{DurationSource: "native", UsageSession: ops.UsageSession{Harness: "claude", UsageCounters: ops.UsageCounters{EstimatedCostUSD: &zero, DurationMS: &ms}}}
	var p usagePage
	got := map[string]string{}
	for _, f := range p.SessionCounters(row) {
		got[f.Label] = f.Value
	}
	if got["Estimated cost"] != "Unknown" {
		t.Errorf("estimated cost = %q", got["Estimated cost"])
	}
	if _, ok := got["Duration (reported by the harness, may include earlier runs)"]; !ok {
		t.Errorf("labels = %v", got)
	}
	in := int64(5)
	row.InputTokens = &in
	for _, f := range p.SessionCounters(row) {
		if f.Label == "Estimated cost" && f.Value != "$0.0000" {
			t.Errorf("with tokens known the zero stays: %q", f.Value)
		}
	}
	if m := p.ModelText(ops.UsageSessionRow{UsageSession: ops.UsageSession{Harness: "codex"}}); !strings.Contains(m, "Codex") {
		t.Errorf("model text %q", m)
	}
}

// Details reads the live rows of the one session directly, in one call.
func TestLiveSessionUsageLooksUpTheSessionDirectly(t *testing.T) {
	start := int64(1700000000000)
	row := ops.UsageSessionRow{WorkerID: "w1", LastSeen: start + 5000, UsageSession: ops.UsageSession{Name: "mt", Harness: "claude", StartedAt: &start}}
	f := newFakeOps().reply(ops.OpUsageList, ops.UsageListResult{Sessions: []ops.UsageSessionRow{row}})
	r := httptest.NewRequest("GET", "/", nil)
	facts, note := liveSessionUsage(r, f, SessionContext{Online: true, WorkerID: "w1", Session: "mt", SessionCreated: 1700000000})
	if note != "" || len(facts) == 0 {
		t.Fatalf("facts=%v note=%q", facts, note)
	}
	calls := f.called(ops.OpUsageList)
	if len(calls) != 1 || calls[0].Args["live_session"] != "mt" || calls[0].Args["worker_id"] != "w1" || calls[0].Args["section"] != "sessions" {
		t.Fatalf("calls = %+v", calls)
	}
	// A page that does not hold every live row is not trusted.
	f = newFakeOps().reply(ops.OpUsageList, ops.UsageListResult{Sessions: []ops.UsageSessionRow{row}, Next: "x"})
	if facts, note := liveSessionUsage(r, f, SessionContext{Online: true, WorkerID: "w1", Session: "mt", SessionCreated: 1700000000}); facts != nil || note == "" {
		t.Fatalf("partial page: facts=%v note=%q", facts, note)
	}
}
