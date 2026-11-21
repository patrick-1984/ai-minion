package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

type usageHostFixture struct{ sessions []term.Session }

func (h usageHostFixture) ListSessions(context.Context) ([]term.Session, error) {
	return h.sessions, nil
}

func TestUsageProjectionDropsTaskTextAndCommandArguments(t *testing.T) {
	host := usageHostFixture{[]term.Session{{Name: "private-build", Task: "PRIVATE sample task payload", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Panes: []term.Pane{{Active: true, Ref: term.PaneRef{Command: "codex -- PRIVATE invocation", PID: 791}}}}}}
	r, err := usageSnapshot(context.Background(), host, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sessions) != 1 || r.Sessions[0].Name != "private-build" || r.Sessions[0].StartedAt == nil || r.Sessions[0].InputTokens != nil || r.Sessions[0].AuthMode != "unknown" {
		t.Fatalf("unexpected projection: %+v", r)
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"PRIVATE", "791", "command", "screen", "transcript", "prompt"} {
		if strings.Contains(string(body), field) {
			t.Fatalf("extra field/content: %s", field)
		}
	}
}
