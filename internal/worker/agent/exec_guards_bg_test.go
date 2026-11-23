package agent

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// A busy state caused only by background work behind an idle prompt does not
// refuse typed input; a real busy turn still does.
func TestInputGuardsBackgroundWork(t *testing.T) {
	mk := func(bg bool) *pane {
		p := &pane{class: agentdetect.Result{Kind: "agent:claude", State: ops.PaneStateBusy, Background: bg}}
		p.info.State = ops.PaneStateBusy
		return p
	}
	if got := inputGuards(mk(true)); len(got) != 0 {
		t.Errorf("background busy: guards %v", got)
	}
	if got := inputGuards(mk(false)); len(got) != 1 {
		t.Errorf("real busy: guards %v", got)
	}
}
