package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestSessionsTableShowsTheCodeFirst(t *testing.T) {
	op, ok := ops.Default.Lookup(ops.OpSessionsList)
	if !ok {
		t.Fatal("no sessions_list")
	}
	raw, _ := json.Marshal(ops.SessionsListResult{Sessions: []ops.SessionInfo{
		{Code: "5-667", Name: "minion-1", Windows: 1},
		{Name: "local", Windows: 2},
	}})
	var b bytes.Buffer
	if err := (printer{w: &b}).value(op, raw); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("output:\n%s", b.String())
	}
	for i, want := range []string{"CODE", "5-667", "-"} {
		if f := strings.Fields(lines[i]); len(f) == 0 || f[0] != want {
			t.Errorf("line %d = %q, want first column %q", i, lines[i], want)
		}
	}
}
