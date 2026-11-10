//go:build unix

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/oldlocal"
)

// TestMCPToolsAgainstAnOlderWorkerDaemon: the MCP server's local backend is the
// shared client, so a tool call that relies on a safeguard is refused by an older
// daemon before anything reaches its terminal, and the model sees an error, never
// "delivered".
func TestMCPToolsAgainstAnOlderWorkerDaemon(t *testing.T) {
	d := oldlocal.New(t)
	c := &caller{backend: d.Backend, log: nopLogger()}
	for name, tc := range map[string]struct {
		op   string
		args string
	}{
		"send_text submit": {ops.OpSendText, `{"target":{"session_name":"work"},"text":"hello","submit":true}`},
		"multi-line text":  {ops.OpSendText, "{\"target\":{\"session_name\":\"work\"},\"text\":\"a\\nb\"}"},
		"answer": {ops.OpAnswer, `{"target":{"session_name":"work"},"choice":"1","expect":{"pane_id":"%1","pid":4242,"command":"claude","session_created":1700000000,"dialog_digest":"` +
			strings.Repeat("ab", 32) + `"}}`},
	} {
		res := c.call(context.Background(), mustTool(t, tc.op), json.RawMessage(tc.args))
		b, _ := json.Marshal(res)
		if !res.IsError || strings.Contains(string(b), "delivered") || !strings.Contains(string(b), "older than this client") {
			t.Errorf("%s: %s", name, b)
		}
	}
	if runs := d.Runs(); len(runs) != 0 {
		t.Fatalf("the older daemon ran %d guarded requests: %+v", len(runs), runs)
	}
	res := c.call(context.Background(), mustTool(t, ops.OpSendText), json.RawMessage(`{"target":{"session_name":"work"},"text":"plain line"}`))
	if res.IsError {
		b, _ := json.Marshal(res)
		t.Fatalf("a plain text: %s", b)
	}
	if runs := d.Runs(); len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
}
