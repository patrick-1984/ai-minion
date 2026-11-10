package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// stubBackend answers every op with a canned result and records the ops.
type stubBackend struct {
	mu   sync.Mutex
	seen []string
	out  map[string]string
}

func (s *stubBackend) Call(_ context.Context, op string, _ any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, op)
	return json.RawMessage(s.out[op]), nil
}

func (s *stubBackend) Stream(ctx context.Context, op string, args any, _ func(json.RawMessage) error) (json.RawMessage, error) {
	return s.Call(ctx, op, args)
}

func (s *stubBackend) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func mcpRoot(deps mcpDeps) *cobra.Command {
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newMCPCmd(deps))
	return root
}

func testMCPDeps(env map[string]string, b client.Backend, trust mcp.Trust) (mcpDeps, *lockedBuf) {
	stderr := &lockedBuf{}
	if _, ok := env["MINION_STATE_DIR"]; !ok {
		env["MINION_STATE_DIR"] = "" // no config file
	}
	return mcpDeps{
		stderr:    stderr,
		lookupEnv: envFrom(env),
		backend:   func() (client.Backend, mcp.Trust) { return b, trust },
	}, stderr
}

// rpc drives "minion mcp" like a harness: it sends the handshake and the given
// requests over stdin, collects the responses by id from stdout and then
// closes stdin.
func rpc(t *testing.T, deps mcpDeps, args []string, requests ...string) (map[int]json.RawMessage, string) {
	t.Helper()
	root := mcpRoot(deps)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	root.SetIn(inR)
	root.SetOut(outW)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"mcp"}, args...))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx); _ = outW.Close() }()

	lines := append([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"h","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}, requests...)
	go func() { _, _ = io.WriteString(inW, strings.Join(lines, "\n")+"\n") }()

	want := 1
	for _, r := range requests {
		if strings.Contains(r, `"id"`) {
			want++
		}
	}
	got := map[int]json.RawMessage{}
	var raw bytes.Buffer
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for len(got) < want && sc.Scan() {
		raw.Write(sc.Bytes())
		raw.WriteByte('\n')
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg.JSONRPC != "2.0" {
			t.Fatalf("stdout carries something that is not a JSON-RPC message: %q", sc.Text())
		}
		if msg.ID != nil {
			got[*msg.ID] = msg.Result
		}
	}
	_ = inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("minion mcp: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("minion mcp did not stop at end of input")
	}
	return got, raw.String()
}

const listRequest = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

func toolNamesOf(t *testing.T, result json.RawMessage) map[string]bool {
	t.Helper()
	var l struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &l); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range l.Tools {
		names[tl.Name] = true
	}
	return names
}

func TestMCPIsReadOnlyByDefault(t *testing.T) {
	deps, _ := testMCPDeps(map[string]string{}, &stubBackend{}, mcp.TrustAll)
	got, _ := rpc(t, deps, nil, listRequest)
	names := toolNamesOf(t, got[2])
	if !names["sessions_list"] || !names["session_snapshot"] || !names["pipe_read"] {
		t.Fatalf("read tools missing: %v", names)
	}
	for _, w := range []string{"send_text", "send_keys", "session_create", "set_task", "answer", "pipe_send"} {
		if names[w] {
			t.Errorf("write tool %s is exposed without --allow-writes", w)
		}
	}
}

func TestMCPAllowWritesBySwitchOrEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want bool
	}{
		{"flag", nil, []string{"--allow-writes"}, true},
		{"flag with value", nil, []string{"--allow-writes=true"}, true},
		{"environment", map[string]string{"MINION_MCP_ALLOW_WRITES": "true"}, nil, true},
		{"environment false", map[string]string{"MINION_MCP_ALLOW_WRITES": "false"}, nil, false},
		{"flag beats environment", map[string]string{"MINION_MCP_ALLOW_WRITES": "true"}, []string{"--allow-writes=false"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range c.env {
				env[k] = v
			}
			deps, stderr := testMCPDeps(env, &stubBackend{}, mcp.TrustAll)
			got, _ := rpc(t, deps, c.args, listRequest)
			names := toolNamesOf(t, got[2])
			if names["send_text"] != c.want || names["pipe_send"] != c.want || !names["sessions_list"] {
				t.Fatalf("tools = %v, want writes=%v", names, c.want)
			}
			if warned := strings.Contains(stderr.String(), "write tools"); warned != c.want {
				t.Errorf("write warning on stderr = %v, want %v (%s)", warned, c.want, stderr.String())
			}
		})
	}
}

func TestMCPInvalidSettingIsRefused(t *testing.T) {
	deps, _ := testMCPDeps(map[string]string{"MINION_MCP_ALLOW_WRITES": "maybe"}, &stubBackend{}, mcp.TrustAll)
	root := mcpRoot(deps)
	root.SetArgs([]string{"mcp"})
	root.SetIn(strings.NewReader(""))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "mcp_allow_writes") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPToolCallRunsThroughTheBackend(t *testing.T) {
	b := &stubBackend{out: map[string]string{ops.OpSessionsList: `{"sessions":[{"name":"work","created":"2026-11-09T10:00:00.000Z","windows":1,"attached":false,"pane":null}]}`}}
	deps, _ := testMCPDeps(map[string]string{}, b, mcp.TrustAll)
	got, raw := rpc(t, deps, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)
	if !strings.Contains(string(got[2]), `"structuredContent"`) || !strings.Contains(string(got[2]), `"work"`) {
		t.Fatalf("tools/call = %s\nstdout:\n%s", got[2], raw)
	}
	if o := b.ops(); len(o) != 1 || o[0] != ops.OpSessionsList {
		t.Fatalf("backend ops = %v", o)
	}
}

func TestMCPWithoutWorkerAnswersWithAToolError(t *testing.T) {
	// The default backend looks for the Worker's socket; none exists here.
	deps, _ := testMCPDeps(map[string]string{}, nil, nil)
	deps.backend = func() (client.Backend, mcp.Trust) {
		return client.Local(client.WithSocketPath(t.TempDir() + "/none.sock")), mcp.TrustAll
	}
	got, _ := rpc(t, deps, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sessions_list","arguments":{}}}`)
	if !strings.Contains(string(got[2]), `"isError":true`) {
		t.Fatalf("tools/call = %s", got[2])
	}
}

func TestMCPKeyFileWithoutAHubURLIsAConfigurationErrorNotTheLocalWorker(t *testing.T) {
	// The Hub backend (Story 6.9) replaced the old "local Worker only" warning:
	// a key file without a Hub address is refused, and never ignored in favour
	// of the local Worker.
	b := &stubBackend{}
	deps, _ := testMCPDeps(map[string]string{"MINION_API_KEY_FILE": "/nonexistent/key"}, b, mcp.TrustAll)
	root := mcpRoot(deps)
	root.SetIn(strings.NewReader(""))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"mcp"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "MINION_HUB_URL") || strings.Contains(err.Error(), "/nonexistent/key") {
		t.Fatalf("error = %v", err)
	}
	if len(b.ops()) != 0 {
		t.Error("the local backend was used")
	}
}

func TestMCPCommandIsRegisteredWithItsFlag(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"mcp"})
	if err != nil || cmd.Name() != "mcp" {
		t.Fatalf("mcp command not registered: %v", err)
	}
	f := cmd.Flags().Lookup("allow-writes")
	if f == nil || !strings.Contains(f.Usage, "MINION_MCP_ALLOW_WRITES") {
		t.Fatalf("--allow-writes flag = %+v", f)
	}
	if _, ok := config.Default.Lookup(config.KeyMCPAllowWrites); !ok {
		t.Fatal("mcp_allow_writes is not declared")
	}
}
