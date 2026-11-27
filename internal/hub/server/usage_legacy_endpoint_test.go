package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A report of a v0.20-dev Worker carries the endpoint host and a label with it.
// The authenticated API must never return it, whether the row was stored
// before the redaction or is saved now.
func TestUsageHTTPNeverReturnsALegacyEndpointHost(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	rootID := d.userID("root")
	root, _ := d.login("root")
	worker := d.enrol("root-workstation", rootID, store.WorkerKindPrivate)
	now := time.Now()
	at := now.UnixMilli()
	id := ops.UsageID("account", "gateway")
	report := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: at, Complete: true,
		Sessions: []ops.UsageSession{{ID: ops.UsageID("session", "gw"), Name: "gw", AuthMode: "api", AccountID: id, Endpoint: "gateway.internal.example:8443", StartedAt: &at, ObservedAt: at, Source: "test"}},
		Accounts: []ops.UsageAccount{{ID: id, Provider: "Anthropic", Label: "Custom endpoint gateway.internal.example", AuthMode: "api", EndpointOrigin: "https://gateway.internal.example:8443", ObservedAt: at, Source: "session-host"}}}
	if err := d.st.SaveUsage(context.Background(), worker, rootID, report, now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.DB().Exec(`UPDATE usage_accounts SET body=json_set(body,'$.endpoint_origin','https://gateway.internal.example:8443','$.label','Custom endpoint gateway.internal.example')`); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"accounts", "sessions"} {
		code, body, _ := d.hc.do(http.MethodPost, ops.RESTPrefix+ops.OpUsageList, ops.UsageListArgs{Section: section}, root)
		if code != http.StatusOK || strings.Contains(body, "gateway.internal") {
			t.Fatalf("%s: %d %s", section, code, body)
		}
		if section == "accounts" && !strings.Contains(body, "Anthropic custom endpoint (not told apart)") {
			t.Fatalf("generic label missing: %s", body)
		}
	}
}
