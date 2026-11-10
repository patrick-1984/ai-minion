package mcp

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const shareGateWorker = "01900000-0000-7000-8000-000000000001"

// share_set changes what the Hub may see of every session of a Worker. The
// default MCP server is read-only: the tool is not offered, a call to it is
// refused by the protocol and never reaches the backend, and no tool that changes
// state carries a read-only hint. share_get, which only reads, stays.
func TestReadOnlyMCPServerDoesNotOfferOrRunShareSet(t *testing.T) {
	fb := newFake()
	cs := connect(t, Config{Backend: fb})
	tools := listTools(t, cs)
	byName := map[string]*sdk.Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	if _, ok := byName[ops.OpShareSet]; ok {
		t.Fatal("the read-only MCP server offers share_set")
	}
	get, ok := byName[ops.OpShareGet]
	if !ok || get.Annotations == nil || !get.Annotations.ReadOnlyHint {
		t.Fatalf("share_get = %+v, want a read-only tool", get)
	}
	for _, op := range ops.All() {
		if tl, ok := byName[op.MCPTool()]; ok && op.ChangesState() {
			t.Errorf("the read-only server offers %s, which changes state (read-only hint %v)", op.Name, tl.Annotations.ReadOnlyHint)
		}
	}

	// Calling it anyway: refused, nothing reaches the backend.
	_, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: ops.OpShareSet, Arguments: map[string]any{
		"worker_id": shareGateWorker, "mode": "all",
	}})
	if err == nil {
		t.Log("the call returned a tool error")
	}
	if fb.callCount() != 0 {
		t.Fatalf("a call to share_set reached the backend in read-only mode: %d calls", fb.callCount())
	}
}

// With writes allowed share_set is offered, is not marked read-only, and runs:
// the arguments reach the backend exactly as given (the Worker, not this server,
// decides whether the id is its own).
func TestShareSetRunsWhenWritesAreAllowedAndIsNotReadOnly(t *testing.T) {
	fb := newFake()
	fb.results[ops.OpShareSet] = json.RawMessage(`{"rule":{"mode":"all"},"unshared":[]}`)
	cs := connect(t, Config{Backend: fb, AllowWrites: true})
	var set *sdk.Tool
	for _, tl := range listTools(t, cs) {
		if tl.Name == ops.OpShareSet {
			set = tl
		}
	}
	if set == nil {
		t.Fatal("share_set is not offered with writes allowed")
	}
	if set.Annotations == nil || set.Annotations.ReadOnlyHint {
		t.Fatalf("share_set is marked read-only: %+v", set.Annotations)
	}
	res := callTool(t, cs, ops.OpShareSet, map[string]any{"worker_id": shareGateWorker, "mode": "all"})
	if res.IsError {
		t.Fatalf("share_set = %+v", res)
	}
	last := fb.last(t)
	var got ops.ShareSetArgs
	if last.op != ops.OpShareSet || json.Unmarshal(last.args, &got) != nil || got.WorkerID != shareGateWorker || got.Mode != "all" {
		t.Fatalf("backend call = %s %s", last.op, last.args)
	}
	// The Hub's own member is not an argument a model can set.
	for _, tl := range listTools(t, cs) {
		if tl.Name != ops.OpShareSet && tl.Name != ops.OpShareGet {
			continue
		}
		schema, _ := json.Marshal(tl.InputSchema)
		if json.Valid(schema) && containsKey(schema, "with_revision") {
			t.Errorf("%s offers the Hub-only member with_revision: %s", tl.Name, schema)
		}
	}
}

func containsKey(schema []byte, key string) bool {
	var m map[string]any
	if json.Unmarshal(schema, &m) != nil {
		return false
	}
	props, _ := m["properties"].(map[string]any)
	_, ok := props[key]
	return ok
}
