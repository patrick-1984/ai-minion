package store

import "github.com/patrick-1984/ai-minion/internal/ops"

// AggregateAPIAccount is the one grouping row of the sessions of a provider
// that use an API key (custom false) or a custom endpoint (custom true). The
// Worker never reads a key or tells endpoints apart, so the row says so
// instead of posing as one credential. It holds no key material, no host and
// no rate-limit window: only token and cost totals belong to it.
func AggregateAPIAccount(provider string, custom bool, at int64) ops.UsageAccount {
	label, kind := provider+" API key (not told apart)", "api_key"
	if custom {
		label, kind = provider+" custom endpoint (not told apart)", "custom_endpoint"
	}
	return ops.UsageAccount{ID: ops.UsageID("session-access", provider, kind), Provider: provider, Label: label, CustomEndpoint: custom, AuthMode: "api", Source: "session-host", ObservedAt: at, TimeSource: "worker_measured"}
}
