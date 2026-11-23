package ops

import (
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
