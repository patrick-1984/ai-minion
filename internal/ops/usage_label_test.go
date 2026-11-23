package ops

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUsageReportKeepsLongEmailLabelAndDropsOnlyABadLabel(t *testing.T) {
	now := time.Now()
	acc := UsageAccount{ID: UsageID("fixture", "l"), Provider: "Anthropic", AuthMode: "subscription", Source: "native-account", ObservedAt: now.UnixMilli(),
		Label: strings.Repeat("a", 64) + "@" + strings.Repeat("b", 60) + ".example.com" + " (login)"}
	r := UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Accounts: []UsageAccount{acc}}
	if err := r.Check(now); err != nil {
		t.Fatalf("long e-mail label rejected: %v", err)
	}
	acc.Label = strings.Repeat("x", UsageLabelMax+1)
	r.Accounts = []UsageAccount{acc}
	if r.Check(now) == nil {
		t.Fatal("oversized label accepted")
	}
	got := r.DropBadLabels()
	if got.Accounts[0].Label != "" || r.Accounts[0].Label == "" {
		t.Fatalf("label not dropped on a copy: %q / %q", got.Accounts[0].Label, r.Accounts[0].Label)
	}
	if err := got.Check(now); err != nil {
		t.Fatalf("report invalid after dropping the label: %v", err)
	}
}

func TestUsageSanitizeDropsAnAccountNeverTheReport(t *testing.T) {
	now := time.Now()
	acct := func(n int) UsageAccount {
		return UsageAccount{ID: UsageID("fixture", strconv.Itoa(n)), Provider: "Anthropic", AuthMode: "api", Source: "session-host", ObservedAt: now.UnixMilli()}
	}
	r := UsageReport{Version: 1, ObservedAt: now.UnixMilli()}
	for i := range 300 { // more than the 128 of v0.19, within what 256 sessions plus native rows need
		r.Accounts = append(r.Accounts, acct(i))
	}
	bad := acct(1000)
	bad.Provider = ""
	r.Accounts = append(r.Accounts, bad, r.Accounts[0])
	got := r.Sanitize()
	if err := got.Check(now); err != nil || len(got.Accounts) != 300 {
		t.Fatalf("sanitized report: %d accounts, %v", len(got.Accounts), err)
	}
	for i := range 400 {
		r.Accounts = append(r.Accounts, acct(2000+i))
	}
	if got := r.Sanitize(); len(got.Accounts) != UsageAccountsMax || got.Check(now) != nil {
		t.Fatalf("over the cap: %d accounts", len(got.Accounts))
	}
}

func TestUsageSessionIgnoresTheDeprecatedEndpointHost(t *testing.T) {
	var s UsageSession
	if err := json.Unmarshal([]byte(`{"id":"x","name":"n","endpoint":"cpa.internal.example:8443"}`), &s); err != nil {
		t.Fatal(err)
	}
	s.DropEndpoint()
	b, _ := json.Marshal(s)
	if !s.CustomEndpoint || strings.Contains(string(b), "cpa.internal") {
		t.Fatalf("endpoint host kept: %s", b)
	}
}
