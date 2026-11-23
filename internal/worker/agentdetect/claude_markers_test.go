package agentdetect

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// claude_idle_2_1_330 is a screen captured from a real idle Claude Code 2.1.330
// session; claude_busy_2_1_330 is the same layout with a spinner line.
func TestClaude2_1_330ScreensAreIdleAndBusy(t *testing.T) {
	for name, want := range map[string]string{
		"claude_idle_2_1_330": ops.PaneStateStable,
		"claude_busy_2_1_330": ops.PaneStateBusy,
		"claude_idle":         ops.PaneStateStable, // older boxed prompt still works
		"claude_busy":         ops.PaneStateBusy,
	} {
		if got := classify(t, Input{Command: "claude", Screen: screen(t, name)}, nil).State; got != want {
			t.Errorf("%s: state %q, want %q", name, got, want)
		}
	}
}
