package agentdetect

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestClaudeBackgroundMarkerIgnoresOutputAbovePrompt(t *testing.T) {
	scr := ops.Screen{Lines: []string{
		"✻ Worked for 23s · done 10:50 PM · 1 shell still running",
		"● The task finished.",
		"────────────────────────────────────────",
		"❯ continue",
		"────────────────────────────────────────",
		"  ⏵⏵ auto mode on · ← 2 agents · ↓ to manage",
	}}
	if ClaudeBackgroundWork(scr) {
		t.Fatal("historical background marker was treated as current")
	}
}

func TestActiveForegroundSpinnerDoesNotBecomeBackgroundOnly(t *testing.T) {
	scr := ops.Screen{Lines: []string{
		"✻ Thinking… (12s · 2 tokens)",
		"────────────────────────────────────────",
		"❯ continue",
		"────────────────────────────────────────",
		"  ⏵⏵ auto mode on · 3 background tasks · ↓ to manage",
	}}
	r := classify(t, Input{Command: "claude", Screen: scr}, nil)
	if r.State != ops.PaneStateBusy || r.Background {
		t.Fatalf("foreground work was treated as background-only: %+v", r)
	}
}

func TestClaudeFooterDetectsOnlyUnfinishedWorkflowProgress(t *testing.T) {
	for _, tc := range []struct {
		row     string
		working bool
	}{
		{"  ◯ release  ▱▱▱ 0/1 · 1h02m", true},
		{"  ✓ release  ▱▱▱ 1/1 · 1h02m", false},
	} {
		scr := ops.Screen{Lines: []string{
			"────────────────────────────────────────",
			"❯ continue",
			"────────────────────────────────────────",
			tc.row,
		}}
		if got := ClaudeBackgroundWork(scr); got != tc.working {
			t.Errorf("%q: background work = %v, want %v", tc.row, got, tc.working)
		}
	}
}
