package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// notes add MCP-specific behaviour to the catalog summary of an op. They are
// appended to the description, which otherwise is the catalog Summary.
var notes = map[string]string{
	ops.OpAnswer:   "A Worker that is older than this server and lacks the reviewed-answer safeguard refuses the call before anything is sent; update and restart the Worker.",
	ops.OpSendText: "With submit, or a text of more than one line, a Worker that is older than this server and lacks the safeguard refuses the call before anything is sent; update and restart the Worker.",
	ops.OpPipeSend: "A text of more than one line to a Worker that is older than this server and lacks the verbatim paste is refused before anything is sent.",
	ops.OpPipeWait: "On MCP it returns as soon as the first turn arrives, or when the wait ends without one; call it again with after_turn_id for more.",
}

// tool is one exposed op: its MCP definition and the resolved input schema.
type tool struct {
	op       ops.Op
	def      *sdk.Tool
	resolved *jsonschema.Resolved
}

// buildTools derives one tool per op. Write ops are left out unless
// allowWrites is set. The list keeps the order of list (the catalog is sorted
// by name).
func buildTools(list []ops.Op, allowWrites bool) ([]tool, error) {
	var out []tool
	for _, op := range list {
		// The read-only server leaves out every op that changes state: a Write
		// op and an op that Mutates (share_set) alike.
		if op.ChangesState() && !allowWrites {
			continue
		}
		schema, err := inputSchema(op)
		if err != nil {
			return nil, err
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("mcp: resolve schema of op %s: %w", op.Name, err)
		}
		desc := op.Summary
		if n := notes[op.Name]; n != "" {
			desc += " " + n
		}
		closed := false
		def := &sdk.Tool{
			Name:        op.MCPTool(),
			Description: desc,
			InputSchema: schema,
			Annotations: &sdk.ToolAnnotations{
				ReadOnlyHint:  !op.ChangesState(),
				OpenWorldHint: &closed,
			},
		}
		out = append(out, tool{op: op, def: def, resolved: resolved})
	}
	return out, nil
}

// errStopStream ends a streaming op once the tool has what it needs.
var errStopStream = errors.New("mcp: stream complete")

// caller runs the tool calls of one server against a backend.
type caller struct {
	backend client.Backend
	trust   Trust
	log     *slog.Logger
	// serving is the context of the running server. The SDK detaches request
	// contexts from it, so the handler joins them again: shutting the server
	// down must cancel a call that is stuck on the backend.
	serving atomic.Pointer[context.Context]
}

// setServing records the context of a running server.
func (c *caller) setServing(ctx context.Context) { c.serving.Store(&ctx) }

// handler returns the tools/call handler of t.
func (c *caller) handler(t tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if base := c.serving.Load(); base != nil {
			stop := context.AfterFunc(*base, cancel)
			defer stop()
		}
		res := c.call(ctx, t, req.Params.Arguments)
		c.log.Debug("mcp tool call", "op", t.op.Name, "error", res.IsError)
		return res, nil
	}
}

// call validates the arguments, runs the op and shapes the result. Every
// failure is a tool result with IsError set, so that the model sees it and can
// correct itself; a protocol error is reserved for a broken request.
func (c *caller) call(ctx context.Context, t tool, raw json.RawMessage) *sdk.CallToolResult {
	body, args, requestID, err := prepareArgs(t, raw)
	if err != nil {
		return errorResult(err, "")
	}
	switch {
	case t.op.Stream:
		return c.stream(ctx, t, body, args)
	default:
		out, err := c.backend.Call(ctx, t.op.Name, body)
		if err != nil {
			return errorResult(err, requestID)
		}
		return c.shape(ctx, t, args, out, requestID)
	}
}

// prepareArgs checks the raw tool arguments against the input schema and the
// argument type, fills the request id of a write when the model gave none, and
// returns the JSON to send together with the decoded value and, for a write,
// the request id that is sent (the model's or the generated one). The id is
// empty for an op that is not a write.
func prepareArgs(t tool, raw json.RawMessage) (json.RawMessage, any, string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, nil, "", argError("the arguments must be a JSON object")
	}
	var requestID string
	if t.op.Write {
		if id, ok := members["request_id"]; !ok || string(bytes.TrimSpace(id)) == `""` || string(bytes.TrimSpace(id)) == "null" {
			b, _ := json.Marshal(ops.NewUUIDv7())
			members["request_id"] = b
		}
		// A request id that is not a string is rejected by the schema below.
		_ = json.Unmarshal(members["request_id"], &requestID)
	}
	body, err := json.Marshal(members)
	if err != nil {
		return nil, nil, "", argError("the arguments are not valid JSON")
	}

	var generic any
	if err := json.Unmarshal(body, &generic); err != nil {
		return nil, nil, "", argError("the arguments are not valid JSON")
	}
	if err := t.resolved.Validate(&generic); err != nil {
		return nil, nil, "", argError(err.Error())
	}

	args := t.op.NewArgs()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(args); err != nil {
		return nil, nil, "", argError(err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, "", argError("unexpected data after the arguments")
	}
	if v, ok := args.(ops.Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, nil, "", err
		}
	}
	return body, args, requestID, nil
}

// argError is an invalid_argument error in the shape of the Worker's.
func argError(msg string) error {
	return proto.NewError(proto.CodeInvalidArgument, msg)
}

// shape turns the backend result of a non-streaming op into a tool result.
func (c *caller) shape(ctx context.Context, t tool, args any, out json.RawMessage, requestID string) *sdk.CallToolResult {
	switch t.op.Name {
	case ops.OpPipeRead:
		var r ops.PipeReadResult
		if err := json.Unmarshal(out, &r); err != nil {
			return errorResult(fmt.Errorf("%w: pipe_read result: %v", client.ErrProtocol, err), "")
		}
		peer := args.(*ops.PipeReadArgs).Target
		return okResult(shapeRead(c.trusted(ctx, peer), peer, r))
	}
	if t.op.Write {
		var r ops.WriteResult
		if err := json.Unmarshal(out, &r); err != nil {
			return errorResult(fmt.Errorf("%w: write result: %v", client.ErrProtocol, err), requestID)
		}
		// The result tells the model which request id was sent, so that a
		// retry after an unknown outcome can reuse it (AD-15).
		res := okResult(withRequestID(out, requestID))
		// A write that did not deliver is not a success for the model, but the
		// structured content still carries the full contract (AD-15).
		res.IsError = r.Outcome != ops.OutcomeDelivered
		return res
	}
	return okResult(json.RawMessage(out))
}

func (c *caller) trusted(ctx context.Context, peer ops.Target) bool {
	if c.trust == nil {
		return false
	}
	return c.trust.Trusted(ctx, peer)
}

// stream runs a streaming op and returns as soon as the first element has
// arrived, or when the stream ends without one. A model cannot consume a
// stream; it calls again with the cursor of the last element it saw.
func (c *caller) stream(ctx context.Context, t tool, body json.RawMessage, args any) *sdk.CallToolResult {
	var elems []json.RawMessage
	_, err := c.backend.Stream(ctx, t.op.Name, body, func(e json.RawMessage) error {
		elems = append(elems, append(json.RawMessage(nil), e...))
		return errStopStream
	})
	if err != nil && !errors.Is(err, errStopStream) {
		return errorResult(err, "")
	}
	if t.op.Name == ops.OpPipeWait {
		turns := make([]ops.PipeTurn, 0, len(elems))
		for _, e := range elems {
			var turn ops.PipeTurn
			if err := json.Unmarshal(e, &turn); err != nil {
				return errorResult(fmt.Errorf("%w: pipe_wait element: %v", client.ErrProtocol, err), "")
			}
			turns = append(turns, turn)
		}
		peer := args.(*ops.PipeWaitArgs).Target
		return okResult(filterTurns(c.trusted(ctx, peer), peer, turns))
	}
	if elems == nil {
		elems = []json.RawMessage{}
	}
	return okResult(map[string]any{"elements": elems})
}

// okResult returns v as structured content, with the same JSON as text for
// clients that read only content.
func okResult(v any) *sdk.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult(fmt.Errorf("encode result: %w", err), "")
	}
	content := string(b)
	if p, ok := v.(pipeTurns); ok && p.Notice != "" {
		content = p.Notice + "\n" + content
	}
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: content}},
		StructuredContent: json.RawMessage(b),
	}
}

// withRequestID adds the request id of a write as a top-level member of its
// result object. A result that is not an object is returned unchanged.
func withRequestID(out json.RawMessage, requestID string) json.RawMessage {
	var members map[string]json.RawMessage
	if requestID == "" || json.Unmarshal(out, &members) != nil || members == nil {
		return out
	}
	id, _ := json.Marshal(requestID)
	members["request_id"] = id
	b, err := json.Marshal(members)
	if err != nil {
		return out
	}
	return b
}

// errorResult reports err to the model. A *proto.Error keeps its code.
// requestID is the id of the write that failed, or empty for any other call.
// A write fails after its request frame was written when the connection broke
// or the reply was not understood (client.ErrConnectionLost, client.ErrProtocol):
// the Worker may have executed it, so the outcome is unknown and the model
// must retry with the same request id, never blindly with a new one (AD-15).
func errorResult(err error, requestID string) *sdk.CallToolResult {
	body := map[string]any{"message": err.Error()}
	var pe *proto.Error
	if errors.As(err, &pe) {
		body["code"] = string(pe.Code)
		body["message"] = pe.Error()
		if errors.Is(err, client.ErrWorkerTooOld) {
			// The update-needed text of the client names what to do; the code is
			// the Worker's.
			body["message"] = err.Error()
			body["outcome"] = string(ops.OutcomeFailedNotSent)
		}
	}
	msg := body["message"].(string)
	top := map[string]any{"error": body}
	if requestID != "" {
		top["request_id"] = requestID
		msg += " (request_id: " + requestID + ")"
		if errors.Is(err, client.ErrConnectionLost) || errors.Is(err, client.ErrProtocol) {
			body["outcome"] = string(ops.OutcomeUnknown)
			msg += " Outcome unknown: retry with this same request_id."
		}
	}
	b, _ := json.Marshal(top)
	return &sdk.CallToolResult{
		IsError:           true,
		Content:           []sdk.Content{&sdk.TextContent{Text: msg}},
		StructuredContent: json.RawMessage(b),
	}
}
