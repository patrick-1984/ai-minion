package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	if row.InputTokens != nil || row.OutputTokens != nil || row.ContextTokens == nil || *row.ContextTokens != 10 || row.EstimatedCostUSD == nil || *row.EstimatedCostUSD != 0 || len(r.Accounts[0].Windows) != 1 {
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

func TestClaudeStatuslineGolden(t *testing.T) {
	now := time.Unix(1795469156, 0)
	account := ops.UsageAccount{ID: ops.UsageID("test", "claude-login"), Provider: "Anthropic", AuthMode: "subscription", Source: "fixture"}
	fiveHours := ops.UsageWindow{
		Scope: "general", Period: "rolling", DurationMS: new((5 * time.Hour).Milliseconds()),
		UsedPercent: new(18.0), ResetsAt: new(int64(1795482000000)),
	}
	sevenDays := ops.UsageWindow{
		Scope: "general", Period: "rolling", DurationMS: new((7 * 24 * time.Hour).Milliseconds()),
		UsedPercent: new(19.0), ResetsAt: new(int64(1795968000000)),
	}
	wantCounters := ops.UsageCounters{
		CacheWriteTokens: new(int64(49167)),
		ContextTokens:    new(int64(56929)), ContextLimit: new(int64(1000000)),
		EstimatedCostUSD: new(0.01058945), DurationMS: new(int64(56157)), APIDurationMS: new(int64(2723)),
		LinesAdded: new(int64(0)), LinesRemoved: new(int64(0)),
		CacheRequests: new(int64(2)), CacheMisses: new(int64(1)), CacheHitRatio: new(0.5677782856339216),
	}
	wantResponse := &ops.UsageResponseTokens{
		InputTokens: new(int64(4)), OutputTokens: new(int64(215)),
		CacheReadTokens: new(int64(33617)), CacheWriteTokens: new(int64(23308)),
	}
	for _, tc := range []struct {
		file    string
		windows []ops.UsageWindow
	}{
		{"claude-2.1.331-statusline.json", []ops.UsageWindow{fiveHours, sevenDays}},
		{"claude-2.1.331-statusline-20x.json", []ops.UsageWindow{sevenDays}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			opts := NativeUsageOptions{Format: "claude-statusline", Session: "work", Provider: "Anthropic", Account: &account}
			r, err := ParseNativeUsage(raw, opts, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Accounts) != 1 || r.Accounts[0].Source != "claude-statusline" || !reflect.DeepEqual(r.Accounts[0].Windows, tc.windows) {
				t.Fatalf("subscription windows = %+v, want %+v", r.Accounts, tc.windows)
			}
			s := r.Sessions[0]
			if s.AccountID != account.ID || s.AuthMode != "subscription" || s.Model != "claude-haiku-5-5" || s.Version != "2.1.331" ||
				!reflect.DeepEqual(s.UsageCounters, wantCounters) || !reflect.DeepEqual(s.LastResponseUsage, wantResponse) {
				got, _ := json.Marshal(s)
				t.Fatalf("unexpected bound session: %s", got)
			}
			opts.Account = nil
			r, err = ParseNativeUsage(raw, opts, now)
			if err != nil {
				t.Fatal(err)
			}
			if s := r.Sessions[0]; len(r.Accounts) != 0 || s.AccountID != "" || s.AuthMode != "unknown" || s.Model != "claude-haiku-5-5" ||
				!reflect.DeepEqual(s.UsageCounters, wantCounters) || !reflect.DeepEqual(s.LastResponseUsage, wantResponse) {
				t.Fatalf("session without an account lost usage or gained attribution: %+v", r)
			}
			opts.CustomEndpoint = true
			opts.Account = &account // explicit endpoint evidence takes priority over a prior binding
			r, err = ParseNativeUsage(raw, opts, now)
			if err != nil {
				t.Fatal(err)
			}
			if s := r.Sessions[0]; len(r.Accounts) != 1 || s.AccountID == account.ID || s.AccountID != r.Accounts[0].ID || !s.CustomEndpoint || s.AuthMode != "api" ||
				!reflect.DeepEqual(s.Windows, tc.windows) || !reflect.DeepEqual(r.Accounts[0].Windows, tc.windows) {
				t.Fatalf("custom endpoint attribution or windows are wrong: %+v", r)
			}
		})
	}
}

func TestClaudeStatuslineNamedWindows(t *testing.T) {
	now := time.Unix(1795469156, 0)
	account := ops.UsageAccount{ID: ops.UsageID("test", "claude-login"), Provider: "Anthropic", AuthMode: "subscription", Source: "fixture"}
	raw := []byte(`{"session_id":"x","rate_limits":{"next_model":{"used_percentage":0,"resets_at":1795482000},"reset_only":{"resets_at":1795968000},"absent":null,"metadata":"ignored","empty":{}}}`)
	r, err := ParseNativeUsage(raw, NativeUsageOptions{Format: "claude-statusline", Session: "work", Account: &account}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []ops.UsageWindow{
		{Scope: "next_model", Period: "rolling", UsedPercent: new(0.0), ResetsAt: new(int64(1795482000000))},
		{Scope: "reset_only", Period: "rolling", ResetsAt: new(int64(1795968000000))},
	}
	if len(r.Accounts) != 1 || !reflect.DeepEqual(r.Accounts[0].Windows, want) {
		t.Fatalf("named windows = %+v, want %+v", r.Accounts, want)
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
