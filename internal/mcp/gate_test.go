package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeGate allows writes to the session "ok" under rule.
type fakeGate struct{ rule string }

func (g fakeGate) Check(op ops.Op, args json.RawMessage) (string, error) {
	if !op.ChangesState() {
		return "", nil
	}
	if strings.Contains(string(args), `"session_name":"ok"`) {
		return g.rule, nil
	}
	return "", proto.NewError(proto.CodeForbidden, "not in a standing Minion allow rule; ask the owner or use the CLI. Nothing was sent.")
}

// ruleBackend records the client rule of each call.
type ruleBackend struct {
	fakeBackend
	rules []string
}

func (b *ruleBackend) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	b.mu.Lock()
	b.rules = append(b.rules, client.ClientRule(ctx))
	b.mu.Unlock()
	return b.fakeBackend.Call(ctx, op, args)
}

func TestGateRefusesBeforeSendingAndLabelsAllowedWrites(t *testing.T) {
	rule := ops.NewUUIDv7()
	rb := &ruleBackend{fakeBackend: *newFake()}
	rb.results[ops.OpSendText] = json.RawMessage(`{"outcome":"delivered","reason":"","forceable":false,"forced":false,"warnings":[],"pane":null}`)
	cs := connect(t, Config{Backend: rb, AllowWrites: true, Gate: fakeGate{rule: rule}})

	res := callTool(t, cs, ops.OpSendText, map[string]any{"target": map[string]any{"session_name": "other"}, "text": "x"})
	if !res.IsError || !strings.Contains(text(res), "not in a standing Minion allow rule") {
		t.Fatalf("refused call: %s", text(res))
	}
	if rb.callCount() != 0 {
		t.Fatal("a refused write reached the backend")
	}
	res = callTool(t, cs, ops.OpSendText, map[string]any{"target": map[string]any{"session_name": "ok"}, "text": "x"})
	if res.IsError {
		t.Fatalf("allowed call: %s", text(res))
	}
	if len(rb.rules) != 1 || rb.rules[0] != rule {
		t.Fatalf("client rules = %v", rb.rules)
	}
}
