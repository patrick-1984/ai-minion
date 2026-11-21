package agent

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestNativeUsageTranslations(t *testing.T) {
	now := time.Now()
	a := ops.UsageAccount{ID: ops.UsageID("fixture", "private"), Provider: "Anthropic", AuthMode: "subscription", Source: "test"}
	claude := []byte(`{"session_id":"synthetic","model":{"id":"model-x"},"workspace":{"current_dir":"/safe"},"transcript_path":"/private/transcript","cost":{"total_cost_usd":0,"total_duration_ms":999},"context_window":{"context_window_size":100,"total_input_tokens":10},"rate_limits":{"five_hour":{"used_percentage":0}}}`)
	r, err := ParseNativeUsage(claude, NativeUsageOptions{Format: "claude-statusline", Session: "tty", Account: &a}, now)
	if err != nil {
		t.Fatal(err)
	}
	row := r.Sessions[0]
	if row.InputTokens != nil || row.ContextTokens == nil || *row.ContextTokens != 10 || row.EstimatedCostUSD == nil || *row.EstimatedCostUSD != 0 || len(r.Accounts[0].Windows) != 1 {
		t.Fatalf("Claude source semantics: %+v", r)
	}
	b, _ := json.Marshal(r)
	if bytes.Contains(b, []byte("/private/transcript")) {
		t.Fatal("private document path leaked")
	}
	codex := []byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"thread","tokenUsage":{"total":{"inputTokens":90,"cachedInputTokens":10,"outputTokens":30},"last":{"inputTokens":2}}}}`)
	r, err = ParseNativeUsage(codex, NativeUsageOptions{Format: "codex-token-usage", Session: "tty"}, now)
	if err != nil || *r.Sessions[0].InputTokens != 90 {
		t.Fatalf("Codex: %v", err)
	}
	gem := []byte(`{"response":"PRIVATE OUTPUT","stats":{"models":{"a":{"tokens":{"prompt":10,"candidates":5,"cached":2}},"b":{"tokens":{"prompt":8,"candidates":3}}}}}`)
	r, err = ParseNativeUsage(gem, NativeUsageOptions{Format: "gemini-result", Session: "tty", NativeID: "run"}, now)
	if err != nil || *r.Sessions[0].InputTokens != 18 || *r.Sessions[0].OutputTokens != 8 || r.Sessions[0].CacheReadTokens != nil || len(r.Sessions[0].Models) != 2 {
		t.Fatalf("Gemini metrics: %+v %v", r, err)
	}
	a.Provider = "Google"
	r, err = ParseNativeUsage([]byte(`{"buckets":[{"modelId":"gemini-pro","remainingFraction":0.75,"resetTime":"2026-12-01T00:00:00Z","tokenType":"REQUESTS"},{"modelId":"gemini-fast"}]}`), NativeUsageOptions{Format: "gemini-quota", Session: "stats", NativeID: "run", Account: &a}, now)
	if err != nil || *r.Accounts[0].Windows[0].UsedPercent != 25 || r.Accounts[0].Windows[1].UsedPercent != nil || r.Accounts[0].Windows[0].DurationMS != nil {
		t.Fatalf("Gemini quota: %+v %v", r, err)
	}
}

func TestClaudeGatewayBudgetDoesNotBecomeSubscriptionQuota(t *testing.T) {
	now := time.Now()
	a := ops.UsageAccount{ID: ops.UsageID("test", "old-subscription"), Provider: "Anthropic", AuthMode: "subscription", Source: "fixture"}
	raw := []byte(`{"session_id":"fixture","cost":{"total_api_duration_ms":45,"total_lines_added":0,"total_lines_removed":7},"prompt_cache":{"requests":5,"misses":0,"hit_ratio":0.9},"rate_limits":{"spend_limit":{"used_percentage":112.5,"resets_at":1830000000,"used_usd":48.7,"limit_usd":50,"period":"monthly"}}}`)
	r, err := ParseNativeUsage(raw, NativeUsageOptions{Format: "claude-statusline", Session: "work", Account: &a}, now)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Sessions[0]
	b := s.GatewaySpend
	if len(r.Accounts) != 0 || s.AccountID != "" || s.AuthMode != "unknown" {
		t.Fatal("gateway-only budget classified as subscription")
	}
	if b == nil || b.UsedPercent == nil || *b.UsedPercent != 112.5 || b.EstimatedUsedUSD == nil || *b.EstimatedUsedUSD != 48.7 || b.LimitUSD == nil || *b.LimitUSD != 50 || b.Period != "monthly" || b.ResetsAt == nil || *b.ResetsAt != 1830000000000 {
		t.Fatalf("budget: %+v", b)
	}
	if *s.APIDurationMS != 45 || *s.LinesAdded != 0 || *s.LinesRemoved != 7 || *s.CacheRequests != 5 || *s.CacheMisses != 0 || *s.CacheHitRatio != .9 || s.CostUSD != nil {
		t.Fatal("native session counter translation")
	}
	r, err = ParseNativeUsage([]byte(`{"session_id":"x","rate_limits":{"spend_limit":{"used_percentage":0}}}`), NativeUsageOptions{Format: "claude-statusline", Session: "work"}, now)
	if err != nil {
		t.Fatal(err)
	}
	b = r.Sessions[0].GatewaySpend
	if b == nil || b.UsedPercent == nil || *b.UsedPercent != 0 || b.EstimatedUsedUSD != nil || b.LimitUSD != nil || b.ResetsAt != nil || b.Period != "" {
		t.Fatalf("unknown gateway amounts invented: %+v", b)
	}
}
