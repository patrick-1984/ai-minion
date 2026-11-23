package server

import (
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The exporter is installed after the first inventory report: the Hub asks
// again soon while it is absent, so the page never keeps the old state.
func TestInventoryRefreshAsksAgainWhileTheExporterIsAbsent(t *testing.T) {
	claude := func(exporter string) []ops.UsageHarness {
		return []ops.UsageHarness{{Name: "claude", Status: "installed", Exporter: exporter}}
	}
	if got := inventoryRefresh(claude(ops.UsageExporterAbsent)); got != time.Minute {
		t.Fatalf("absent: %v", got)
	}
	if got := inventoryRefresh(claude(ops.UsageExporterInstalled)); got != 10*time.Minute {
		t.Fatalf("installed: %v", got)
	}
	if got := inventoryRefresh(nil); got != 10*time.Minute {
		t.Fatalf("none: %v", got)
	}
}
