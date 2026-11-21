package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestMasterToolsFollowCatalogGeneration(t *testing.T) {
	ctx := t.Context()
	server := newMasterServer("1")
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{}, 1)
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) {
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	})
	conn, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	capabilities := conn.InitializeResult().Capabilities
	if capabilities.Tools == nil || !capabilities.Tools.ListChanged {
		t.Fatal("the empty catalog did not negotiate live tool updates")
	}
	var generationSeen uint64
	var argumentsSeen ops.SendTextArgs
	backend := MasterBackend{Call: func(_ context.Context, gen uint64, op, id string, body json.RawMessage) (json.RawMessage, error) {
		generationSeen = gen
		if op == ops.OpSendText {
			if err := json.Unmarshal(body, &argumentsSeen); err != nil {
				return nil, err
			}
			if id != argumentsSeen.RequestID {
				t.Errorf("outer and inner request IDs differ")
			}
			return json.Marshal(ops.WriteResult{Outcome: ops.OutcomeDelivered})
		}
		return json.RawMessage(`{"workers":[]}`), nil
	}}
	var cat masterCatalog
	tools := func() []string {
		t.Helper()
		list, err := conn.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(list.Tools))
		for _, tool := range list.Tools {
			out = append(out, tool.Name)
		}
		slices.Sort(out)
		return out
	}
	if got := tools(); len(got) != 0 {
		t.Fatalf("idle tools = %v", got)
	}
	cat.refresh(ctx, server, 8, backend)
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("the client did not receive a catalog update")
	}
	want := []string{"send_text_g8", "session_snapshot_g8", "sessions_list_g8", "workers_list_g8"}
	if got := tools(); !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	requestID := ops.NewUUIDv7()
	arguments := map[string]any{
		"target": map[string]string{"worker_id": ops.NewUUIDv7(), "session_name": "test"},
		"text":   "Report the fixture status.", "request_id": requestID,
	}
	result, err := conn.CallTool(ctx, &sdk.CallToolParams{Name: "send_text_g8", Arguments: arguments})
	if err != nil || result.IsError {
		t.Fatalf("prompt tool result = %+v, err = %v", result, err)
	}
	if generationSeen != 8 || argumentsSeen.Force || argumentsSeen.AgentOnly || !argumentsSeen.Submit || argumentsSeen.RequestID != requestID {
		t.Fatalf("unsafe or incorrect prompt wire payload: %d %+v", generationSeen, argumentsSeen)
	}
	cat.refresh(ctx, server, 0, backend)
	if got := tools(); len(got) != 0 {
		t.Fatalf("revoked tools = %v", got)
	}
	cat.refresh(ctx, server, 10, backend)
	old, err := conn.CallTool(ctx, &sdk.CallToolParams{Name: "send_text_g8", Arguments: arguments})
	if err == nil && !old.IsError {
		t.Fatalf("old generation unexpectedly callable: %+v", old)
	}
	if generationSeen != 8 {
		t.Fatal("a cached invocation was restamped to the current catalog")
	}
}
