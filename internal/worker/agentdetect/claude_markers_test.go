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

// Background work behind an idle prompt is reported as busy; the prompt still
// takes input, and a bare "← 2 agents" hint is not background work.
func TestClaudeBackgroundWorkIsBusy(t *testing.T) {
	for name, want := range map[string]string{
		"claude_bg_shell_2_1_33x":  ops.PaneStateBusy,
		"claude_bg_agents_2_1_33x": ops.PaneStateBusy,
		"claude_idle_2_1_330":      ops.PaneStateStable, // ends in "← 2 agents"
	} {
		r := classify(t, Input{Command: "claude", Screen: screen(t, name)}, nil)
		if r.State != want {
			t.Errorf("%s: state %q, want %q", name, r.State, want)
		}
		if r.Background != (want == ops.PaneStateBusy) {
			t.Errorf("%s: Background %v", name, r.Background)
		}
		if r.Kind != "agent:claude" {
			t.Errorf("%s: kind %q", name, r.Kind)
		}
	}
}
