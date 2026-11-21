package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// MasterBackend is supplied only by the dedicated, verified local connection.
// A generation labels a catalog and never contains credential material.
type MasterBackend struct {
	Generation func(context.Context) (uint64, error)
	Catalog    func(context.Context, uint64, string)
	Call       func(context.Context, uint64, string, string, json.RawMessage) (json.RawMessage, error)
}

type masterCatalog struct {
	mu         sync.RWMutex
	generation uint64
	names      []string
}

// ServeMaster starts with an empty MCP catalog and has no generic Minion
// backend. Native clients can retain previous declarations, so each registered
// tool closes over its original generation even after they regain access.
func ServeMaster(ctx context.Context, in io.Reader, out io.Writer, version string, backend MasterBackend) error {
	if backend.Generation == nil || backend.Call == nil {
		return errors.New("the Minion master backend is required")
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "minion-master", Version: version}, &sdk.ServerOptions{HasTools: true})
	cat := masterCatalog{}
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			cat.mu.RLock()
			res, err := next(ctx, method, req)
			gen := cat.generation
			cat.mu.RUnlock()
			if backend.Catalog != nil {
				report := proto.MasterCatalogEmpty
				switch {
				case err != nil:
					report = proto.MasterCatalogFailed
				case gen > 0:
					report = proto.MasterCatalogActive
				}
				backend.Catalog(ctx, gen, report)
			}
			return res, err
		}
	})
	ctx, cancel := context.WithCancel(ctx)
	updated := make(chan struct{})
	go func() {
		defer close(updated)
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			gen, err := backend.Generation(ctx)
			if err != nil || gen > proto.MaxMasterGeneration {
				gen = 0
			}
			cat.refresh(ctx, server, gen, backend)
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	err := server.Run(ctx, &sdk.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
	cancel()
	<-updated
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (c *masterCatalog) refresh(ctx context.Context, server *sdk.Server, gen uint64, backend MasterBackend) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation == gen {
		return
	}
	server.RemoveTools(c.names...)
	c.generation, c.names = gen, nil
	if gen != 0 {
		c.names = addMasterTools(ctx, server, gen, backend)
	}
}

type masterSessionsArgs struct {
	WorkerID string `json:"worker_id" jsonschema:"Worker UUID from workers_list."`
}

type masterTarget struct {
	WorkerID    string `json:"worker_id" jsonschema:"Worker UUID from workers_list."`
	SessionName string `json:"session_name" jsonschema:"Session name from sessions_list."`
	Pane        string `json:"pane,omitempty" jsonschema:"Optional window.pane index. Omit for the active pane."`
}

type masterSnapshotArgs struct {
	Target masterTarget `json:"target"`
}

type masterPromptArgs struct {
	Target    masterTarget `json:"target"`
	Text      string       `json:"text" jsonschema:"Ordinary task prompt to deliver to the idle target agent."`
	RequestID string       `json:"request_id,omitempty" jsonschema:"Optional UUIDv7. Omit to generate one. Preserve it if retrying an uncertain result after checking the target."`
}

func addMasterTools(ctx context.Context, srv *sdk.Server, gen uint64, backend MasterBackend) []string {
	return []string{
		addMasterTool(ctx, srv, gen, backend, ops.OpWorkersList,
			"List Workers reachable by this Minion master and find their IDs before listing sessions.",
			func(in struct{}) (any, string) { return in, "" }),
		addMasterTool(ctx, srv, gen, backend, ops.OpSessionsList,
			"List the shared sessions this master can reach on the chosen Worker.",
			func(in masterSessionsArgs) (any, string) { return in, "" }),
		addMasterTool(ctx, srv, gen, backend, ops.OpSessionSnapshot,
			"Inspect the current visible screen of an allowed target. Use this before delivering a task.",
			func(in masterSnapshotArgs) (any, string) { return in, "" }),
		addMasterTool(ctx, srv, gen, backend, ops.OpSendText,
			"Deliver an ordinary prompt to an idle agent after inspecting its screen. Check the delivery outcome and request ID. Review the screen after an unknown result.",
			func(in masterPromptArgs) (any, string) {
				id := in.RequestID
				if id == "" {
					id = ops.NewUUIDv7()
				}
				return ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: ops.Target(in.Target),
					Text: in.Text, Submit: true}, id
			}),
	}
}

func addMasterTool[Input any](base context.Context, srv *sdk.Server, gen uint64, backend MasterBackend, op, desc string, args func(Input) (any, string)) string {
	name := op + "_g" + strconv.FormatUint(gen, 10)
	closed := false
	sdk.AddTool(srv, &sdk.Tool{Name: name, Description: desc, Annotations: &sdk.ToolAnnotations{
		ReadOnlyHint: op != ops.OpSendText, OpenWorldHint: &closed,
	}}, func(ctx context.Context, _ *sdk.CallToolRequest, in Input) (*sdk.CallToolResult, any, error) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(base, cancel)
		defer stop()
		value, reqID := args(in)
		body, err := json.Marshal(value)
		if err != nil {
			return errorResult(argError("the master tool arguments cannot be encoded"), reqID), nil, nil
		}
		id := reqID
		if id == "" {
			id = ops.NewUUIDv7()
		} else if !ops.ValidUUIDv7(id) {
			return errorResult(argError("request_id must be a UUIDv7"), reqID), nil, nil
		}
		out, err := backend.Call(ctx, gen, op, id, body)
		if err != nil {
			return errorResult(err, reqID), nil, nil
		}
		operation, _ := ops.Lookup(op)
		return (&caller{}).shape(ctx, tool{op: operation}, nil, out, reqID), nil, nil
	})
	return name
}
