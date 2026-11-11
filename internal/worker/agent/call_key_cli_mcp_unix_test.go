//go:build unix

package agent_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/cli"
	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

const peerWorkerID = "01890a5d-ac96-774b-bcce-b302099a8057"

// answerOneCall plays the Hub for one call stream: it returns the op.request
// the Worker sent and answers delivered.
func answerOneCall(t *testing.T, ctx context.Context, hub *transport.Session) (op, apiKey string, target ops.Target) {
	t.Helper()
	op, apiKey, target, err := answerOneCallErr2(ctx, hub)
	if err != nil {
		t.Fatalf("no call stream reached the Hub: %v", err)
	}
	return op, apiKey, target
}

func answerOneCallErr(ctx context.Context, hub *transport.Session) (op, apiKey string, target ops.Target) {
	op, apiKey, target, err := answerOneCallErr2(ctx, hub)
	if err != nil {
		panic(err)
	}
	return op, apiKey, target
}

func answerOneCallErr2(ctx context.Context, hub *transport.Session) (op, apiKey string, target ops.Target, _ error) {
	for {
		st, err := hub.Accept(ctx)
		if err != nil {
			return "", "", ops.Target{}, err
		}
		if st.Kind() != transport.KindCall {
			continue
		}
		body, err := st.ReadFrame()
		if err != nil {
			return "", "", ops.Target{}, err
		}
		var rq struct {
			Op     string `json:"op"`
			APIKey string `json:"api_key"`
			Args   struct {
				Target ops.Target `json:"target"`
			} `json:"args"`
		}
		if err := json.Unmarshal(body, &rq); err != nil {
			return "", "", ops.Target{}, err
		}
		if err := st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered}}); err != nil {
			return "", "", ops.Target{}, err
		}
		_ = st.Close()
		return rq.Op, rq.APIKey, rq.Args.Target, nil
	}
}

// F-01: the real local CLI and the real local MCP server reach another Worker
// only through the Worker's call_api_key_file, which is read again on every
// call, so a rotated key takes effect at once.
func TestLocalCLIAndMCPCrossWorkerCallsUseTheCallKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "call.key")
	k1 := identity.NewAPIKey().String()
	if err := identity.WritePrivate(keyFile, []byte(k1+"\n")); err != nil {
		t.Fatal(err)
	}
	w := agent.StartHubModeWorker(t, keyFile)
	hub := w.Hub()
	w.WaitConnected(t)
	// The CLI finds the Worker through the default socket directory.
	t.Setenv("XDG_RUNTIME_DIR", w.RunDir)
	t.Setenv("TMPDIR", w.RunDir)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("MINION_HUB_URL", "")
	t.Setenv("MINION_API_KEY_FILE", "")

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	runCLI := func(text string) (string, string) {
		type res struct {
			code int
			out  string
			err  string
		}
		done := make(chan res, 1)
		go func() {
			var out, errb strings.Builder
			code := cli.Run(ctx, []string{"mn", "pipe_send", "--target", peerWorkerID + ":work", "--text", text}, &out, &errb)
			done <- res{code, out.String(), errb.String()}
		}()
		op, key, tgt := answerOneCall(t, ctx, hub)
		r := <-done
		if r.code != cli.ExitOK || !strings.Contains(r.out, "delivered") {
			t.Fatalf("mn pipe_send = %d stdout %q stderr %q", r.code, r.out, r.err)
		}
		if op != ops.OpPipeSend || tgt.WorkerID != peerWorkerID || tgt.SessionName != "work" {
			t.Fatalf("the Hub got %s for %+v", op, tgt)
		}
		return key, text
	}

	// 1. The local CLI presents the key of the file.
	if key, _ := runCLI("from the cli"); key != k1 {
		t.Fatalf("CLI call carried api_key %q, want the key of call_api_key_file", key)
	}

	// 2. The key file is read again: after a rotation the next call carries the new key.
	k2 := identity.NewAPIKey().String()
	if err := identity.WritePrivate(keyFile, []byte(k2+"\n")); err != nil {
		t.Fatal(err)
	}
	if key, _ := runCLI("after rotation"); key != k2 {
		t.Fatalf("after rotation the CLI call carried %q, want the new key", key)
	}

	// 3. The local MCP server (writes enabled) calls through the same socket
	// and the same key.
	srv, err := mcp.New(mcp.Config{Backend: client.Local(client.WithSocketPath(w.Socket)), AllowWrites: true, Trust: mcp.TrustAll})
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, inR, outW); _ = outW.Close() }()
	send := func(line string) {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"h","version":"1"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pipe_send","arguments":{"request_id":"` + ops.NewUUIDv7() + `","target":{"worker_id":"` + peerWorkerID + `","session_name":"work"},"text":"from mcp"}}}`)
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	hubCtx, hubCancel := context.WithTimeout(ctx, 10*time.Second)
	defer hubCancel()
	reached := make(chan [3]any, 1)
	go func() {
		defer func() { _ = recover() }()
		op, key, tgt := answerOneCallErr(hubCtx, hub)
		reached <- [3]any{op, key, tgt}
	}()
	var reply string
	var op, key string
	var tgt ops.Target
wait:
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, `"id":2`) {
				reply = l
				if strings.Contains(l, `"isError":true`) {
					t.Fatalf("MCP tools/call failed: %s", l)
				}
			}
		case r := <-reached:
			op, key, tgt = r[0].(string), r[1].(string), r[2].(ops.Target)
			break wait
		case <-hubCtx.Done():
			t.Fatalf("the MCP call never reached the Hub; MCP said %q", reply)
		}
	}
	if op != ops.OpPipeSend || key != k2 || tgt.WorkerID != peerWorkerID {
		t.Fatalf("MCP call: op %s api_key %q (want %q) target %+v", op, key, k2, tgt)
	}
	for reply == "" || !strings.Contains(reply, "delivered") {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("MCP closed without an answer")
			}
			if strings.Contains(l, `"id":2`) {
				reply = l
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no MCP answer: %q", reply)
		}
	}
	if !strings.Contains(reply, "delivered") || strings.Contains(reply, `"isError":true`) {
		t.Fatalf("MCP tools/call = %s", reply)
	}
	if err := inW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Logf("MCP server ended with: %v", err)
	}

	// 4. A key file that group or others can read stops the calls (the error
	// names the path, never the key); the Worker itself keeps running.
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	code := cli.Run(ctx, []string{"mn", "pipe_send", "--target", peerWorkerID + ":work", "--text", "x"}, &out, &errb)
	if code == cli.ExitOK {
		t.Fatal("a world-readable call key file was accepted")
	}
	if strings.Contains(out.String()+errb.String(), k2) {
		t.Fatal("the key leaked into the output")
	}
}
