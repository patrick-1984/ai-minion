package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A96: the sessions table shows the harness session name as TITLE, a dash
// when there is none; the JSON carries it as the optional harness_name.
func TestRenderSessionsShowsHarnessTitle(t *testing.T) {
	r := ops.SessionsListResult{Sessions: []ops.SessionInfo{
		{Name: "work", Windows: 1, Pane: &ops.PaneInfo{Kind: "agent:claude", State: "busy", PaneStateDetail: ops.PaneStateDetail{HarnessName: "release prep"}}},
		{Name: "shell", Windows: 1, Pane: &ops.PaneInfo{Kind: "shell", State: "stable"}},
	}}
	var b bytes.Buffer
	renderSessions(&errWriter{w: &b}, r)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 || len(strings.Fields(lines[0])) != 7 || strings.Fields(lines[0])[5] != "TITLE" ||
		!strings.Contains(lines[1], " busy    release prep") || strings.Fields(lines[2])[5] != "-" {
		t.Fatalf("table:\n%s", b.String())
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"harness_name":"release prep"`) || strings.Count(string(raw), "harness_name") != 1 {
		t.Fatalf("json = %s", raw)
	}
}
