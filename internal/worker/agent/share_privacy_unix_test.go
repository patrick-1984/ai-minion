//go:build unix

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Through the real local control socket (what the CLI and a local MCP server
// use): another Worker's id never changes this Worker, and nothing is redirected
// to it. The entry point is the real localExecutor over the real Executor.
func TestTheLocalControlSocketNeverActsOnAnotherWorkersShare(t *testing.T) {
	const foreign = "01900000-0000-7000-8000-0000000000ff"
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "private")
	c := serveLocal(t, localExecutor{ex: e.ex, log: e.ex.log})

	args := shareSetArgs("all")
	args.WorkerID = foreign
	b, err := callLocal(t, c, ops.OpShareSet, args)
	if err == nil || proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("share_set naming another Worker over the local socket: %s %v", b, err)
	}
	if e.saved().Mode != store.ShareNone {
		t.Fatalf("a foreign worker_id changed the local Worker to %+v", e.saved())
	}
	if b, err := callLocal(t, c, ops.OpShareGet, ops.ShareGetArgs{WorkerID: foreign}); err == nil {
		t.Fatalf("share_get naming another Worker answered %s", b)
	}

	// Its own id works through the socket.
	if b, err := callLocal(t, c, ops.OpShareSet, shareSetArgs("all")); err != nil || e.saved().Mode != store.ShareAll {
		t.Fatalf("share_set naming the Worker itself: %s %v, saved %+v", b, err, e.saved())
	}
}

// The same boundary through a local MCP server over the real local socket: with
// writes allowed the model may change the sharing of the Worker it is attached
// to by naming it, and naming another Worker changes nothing; the default
// (read-only) server does not offer the tool at all.
func TestLocalMCPNeverRetargetsShareSetToTheLocalWorker(t *testing.T) {
	const foreign = "01900000-0000-7000-8000-0000000000ff"
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "private")
	c := serveLocal(t, localExecutor{ex: e.ex, log: e.ex.log})

	connect := func(cfg mcp.Config) *stdioMCP {
		srv, err := mcp.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return newStdioMCP(t, srv)
	}

	readOnly := connect(mcp.Config{Backend: c})
	tools, err := readOnly.listTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range tools {
		if name == ops.OpShareSet {
			t.Fatal("the default MCP server offers share_set")
		}
	}
	if isErr, err := readOnly.callTool(ops.OpShareSet, map[string]any{"worker_id": shareWorkerID, "mode": "all"}); err == nil && !isErr {
		t.Fatal("the default MCP server ran share_set")
	}
	if e.saved().Mode != store.ShareNone {
		t.Fatalf("the read-only MCP server changed the sharing: %+v", e.saved())
	}

	writer := connect(mcp.Config{Backend: c, AllowWrites: true})
	if isErr, err := writer.callTool(ops.OpShareSet, map[string]any{"worker_id": foreign, "mode": "all"}); err == nil && !isErr {
		t.Fatal("share_set naming another Worker succeeded")
	}
	if e.saved().Mode != store.ShareNone {
		t.Fatalf("a foreign worker_id through MCP changed the local Worker to %+v", e.saved())
	}
	isErr, err := writer.callTool(ops.OpShareSet, map[string]any{"worker_id": shareWorkerID, "mode": "all"})
	if err != nil || isErr || e.saved().Mode != store.ShareAll {
		t.Fatalf("share_set naming the Worker itself: isError=%v %v, saved %+v", isErr, err, e.saved())
	}
}

// stdioMCP is a bounded newline-delimited JSON-RPC client of the real stdio
// transport (Server.Serve, as `minion mcp` runs it), built on the standard
// library only: the MCP SDK is imported by internal/mcp alone (AD-1).
type stdioMCP struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Reader
	id  int
}

func newStdioMCP(t *testing.T, srv *mcp.Server) *stdioMCP {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, inR, outW); _ = outW.Close(); close(done) }()
	c := &stdioMCP{t: t, in: inW, out: bufio.NewReader(outR)}
	t.Cleanup(func() { _ = inW.Close(); cancel(); _ = outR.Close(); <-done })
	if _, err := c.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return c
}

func (c *stdioMCP) send(msg map[string]any) {
	c.t.Helper()
	line, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(line, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// rpc sends one request and returns its result, or the JSON-RPC error as an
// error. Messages that are not the answer are skipped.
func (c *stdioMCP) rpc(method string, params any) (json.RawMessage, error) {
	c.t.Helper()
	c.id++
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			c.t.Fatalf("%s: no answer: %v", method, err)
		}
		var m struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			c.t.Fatalf("%s: not JSON-RPC: %v", method, err)
		}
		if m.ID == nil || m.Method != "" || *m.ID != c.id {
			continue
		}
		if m.Error != nil {
			return nil, fmt.Errorf("jsonrpc error %d: %s", m.Error.Code, m.Error.Message)
		}
		return m.Result, nil
	}
}

func (c *stdioMCP) listTools() ([]string, error) {
	raw, err := c.rpc("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names, nil
}

// callTool reports the tool's isError flag; err is a protocol error.
func (c *stdioMCP) callTool(name string, args any) (isErr bool, err error) {
	raw, err := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return false, err
	}
	var res struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return false, err
	}
	return res.IsError, nil
}
