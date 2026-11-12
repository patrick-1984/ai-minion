package server

// Story 6.9: the Hub REST backend of the CLI and MCP, against a real in-process
// Hub: the real HTTP stack, the real Bearer authentication, API-key scopes and
// authorisation and the real op dispatcher. Only the Workers are private fakes:
// WorkerRouter is the seam behind the real routes, and the fake answers for the
// Worker the dispatcher already authorised, so a denied call shows as "no
// effect".

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// fakeWorkerRouter is the private fake Worker behind the real routes.
type fakeWorkerRouter struct {
	mu       sync.Mutex
	sessions map[string][]ops.SessionInfo
	typed    []string // "<worker>:<session>:<text>" of every delivered send_text
}

func (f *fakeWorkerRouter) Call(_ context.Context, req workers.CallRequest) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch req.Op {
	case ops.OpSessionsList:
		return ops.SessionsListResult{Sessions: append([]ops.SessionInfo{}, f.sessions[req.WorkerID]...)}, nil
	case ops.OpSendText:
		a := req.Args.(*ops.SendTextArgs)
		f.typed = append(f.typed, req.WorkerID+":"+req.Session+":"+a.Text)
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
	}
	return nil, api.NewError("unsupported_op", "the fake Worker does not implement "+req.Op)
}

func (f *fakeWorkerRouter) Stream(_ context.Context, req workers.CallRequest, emit func(any) error) error {
	for _, id := range []string{"01930000-0000-7000-8000-00000000000a", "01930000-0000-7000-8000-00000000000b"} {
		if err := emit(ops.PipeTurn{TurnID: id, Session: req.Session, Direction: ops.PipeReceived, Text: "hello"}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeWorkerRouter) delivered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed...)
}

var _ api.WorkerRouter = (*fakeWorkerRouter)(nil)

type clientHub struct {
	*apiHub
	router  *fakeWorkerRouter
	caFile  string
	baseURL string
}

// startClientHub starts a Hub with the real API-key module and the real Worker
// op handlers (sessions_list, send_text, pipe_wait) routed to a fake Worker.
func startClientHub(t *testing.T) *clientHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "client-integration-pw-1")
	ch := &clientHub{apiHub: &apiHub{workers: fakeWorkers{}}, router: &fakeWorkerRouter{sessions: map[string][]ops.SessionInfo{}}}
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpAPIKeyCreate, ops.OpAPIKeyList, ops.OpAPIKeyRevoke, ops.OpSessionsList, ops.OpSendText, ops.OpPipeWait} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("no op %s", n)
		}
		cat.Register(op)
	}
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	registerAPI(r, cat)
	err := r.Register("fake-workers", apikeyModuleOrder-1, func(_ context.Context, env *Env) error {
		az, err := Require[*authz.Authorizer](env)
		if err != nil {
			return err
		}
		if err := az.SetWorkerLookup(ch.workers); err != nil {
			return err
		}
		if ch.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if ch.audit, err = Require[*audit.Log](env); err != nil {
			return err
		}
		if ch.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		reg, err := Require[*api.Registry](env)
		if err != nil {
			return err
		}
		for _, name := range []string{ops.OpSessionsList, ops.OpSendText, ops.OpPipeWait} {
			if err := api.RegisterWorkerOp(reg, name, ch.router); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registerAPIKeyModule(r)
	data := hubstore.Dir(t)
	ch.h = startHub(t, &Config{DataDir: data}, r)
	ch.hc = newHubClient(t, ch.h, "https")
	ch.baseURL = "https://" + ch.h.srv.Addr()
	ch.caFile = filepath.Join(data, TLSDir, TLSCertFile)
	if _, err := os.Stat(ch.caFile); err != nil {
		t.Fatalf("the Hub's own certificate is not at %s: %v", ch.caFile, err)
	}
	return ch
}

// keyFile writes key to a private file and returns its path.
func keyFile(t *testing.T, key string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (ch *clientHub) hubConfig(t *testing.T, key, worker string) client.HubConfig {
	return client.HubConfig{URL: ch.baseURL, APIKeyFile: keyFile(t, key), CAFile: ch.caFile, Worker: worker}
}

// mcpSession serves the MCP tools over the Hub backend through the real stdio
// transport (Server.Serve, as `minion mcp` does) and returns a client of them.
// The client is a bounded newline-delimited JSON-RPC peer built on the
// standard library only: the MCP SDK is imported by internal/mcp alone.
type mcpSession struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Reader
	id  int
}

func newMCPSession(t *testing.T, b client.Backend, allowWrites bool) *mcpSession {
	t.Helper()
	srv, err := mcp.New(mcp.Config{Backend: b, AllowWrites: allowWrites, Trust: mcp.TrustAll})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, inR, outW); _ = outW.Close(); close(done) }()
	cs := &mcpSession{t: t, in: inW, out: bufio.NewReader(outR)}
	t.Cleanup(func() { _ = inW.Close(); cancel(); _ = outR.Close(); <-done })
	if _, err := cs.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	cs.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return cs
}

func (cs *mcpSession) send(msg map[string]any) {
	cs.t.Helper()
	line, err := json.Marshal(msg)
	if err != nil {
		cs.t.Fatal(err)
	}
	if _, err := cs.in.Write(append(line, '\n')); err != nil {
		cs.t.Fatal(err)
	}
}

// rpc sends one request and returns its result, or the JSON-RPC error as an
// error. Messages of the server that are not the answer are skipped. A
// stalled server fails the test at the session deadline (the pipe closes).
func (cs *mcpSession) rpc(method string, params any) (json.RawMessage, error) {
	cs.t.Helper()
	cs.id++
	cs.send(map[string]any{"jsonrpc": "2.0", "id": cs.id, "method": method, "params": params})
	for {
		line, err := cs.out.ReadBytes('\n')
		if err != nil {
			cs.t.Fatalf("%s: no answer: %v", method, err)
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
			cs.t.Fatalf("%s: not JSON-RPC: %v", method, err)
		}
		if m.ID == nil || m.Method != "" || *m.ID != cs.id {
			continue
		}
		if m.Error != nil {
			return nil, fmt.Errorf("jsonrpc error %d: %s", m.Error.Code, m.Error.Message)
		}
		return m.Result, nil
	}
}

// callTool calls a tool; err is a protocol error (such as an unknown tool).
func (cs *mcpSession) callTool(tool string, args any) (text string, isErr bool, err error) {
	cs.t.Helper()
	raw, err := cs.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", false, err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		cs.t.Fatalf("CallTool %s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), res.IsError, nil
}

func callMCP(t *testing.T, cs *mcpSession, tool string, args any) (text string, isErr bool) {
	t.Helper()
	text, isErr, err := cs.callTool(tool, args)
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	return text, isErr
}

type clientFixture struct {
	ch                   *clientHub
	wA, wB               string
	readA, operateA, rdB string // keys
}

func newClientFixture(t *testing.T) *clientFixture {
	t.Helper()
	ch := startClientHub(t)
	alice := ch.user(t, "alice")
	f := &clientFixture{ch: ch, wA: ch.worker(alice), wB: ch.worker(alice)}
	ch.router.sessions[f.wA] = []ops.SessionInfo{{Name: "alpha", Windows: 1}}
	ch.router.sessions[f.wB] = []ops.SessionInfo{{Name: "beta", Windows: 1}}
	tok, csrf := ch.login(t, "alice")
	sess := asSession(tok, csrf)
	f.readA, _ = ch.newKey(t, sess, map[string]any{"name": "read A", "resources": workerGrant(f.wA), "actions": []string{"read"}})
	f.operateA, _ = ch.newKey(t, sess, map[string]any{"name": "operate A", "resources": workerGrant(f.wA), "actions": []string{"read", "operate"}})
	f.rdB, _ = ch.newKey(t, sess, map[string]any{"name": "read B", "resources": workerGrant(f.wB), "actions": []string{"read"}})
	return f
}

func TestMCPSessionsListThroughTheRealHubIsFilteredByTheScopedKey(t *testing.T) {
	f := newClientFixture(t)
	hubFor := func(key, worker string) client.Backend {
		b, err := client.NewHub(f.ch.hubConfig(t, key, worker))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	}

	// The key scoped to Worker A lists A's sessions: the real Hub answered the
	// real Bearer request and the fake Worker's list came back through MCP.
	cs := newMCPSession(t, hubFor(f.readA, f.wA), false)
	text, isErr := callMCP(t, cs, "sessions_list", map[string]any{})
	if isErr || !strings.Contains(text, "alpha") || strings.Contains(text, "beta") {
		t.Fatalf("scoped key on its Worker: error %v, %s", isErr, text)
	}
	// The same key asking for Worker B, which it has no scope for and whose
	// sessions the same owner holds: filtered out, as not found.
	cs = newMCPSession(t, hubFor(f.readA, f.wB), false)
	text, isErr = callMCP(t, cs, "sessions_list", map[string]any{})
	if !isErr || strings.Contains(text, "beta") || !strings.Contains(text, "not_found") {
		t.Fatalf("scoped key on another Worker: error %v, %s", isErr, text)
	}
	// B's own key sees B only.
	cs = newMCPSession(t, hubFor(f.rdB, f.wB), false)
	if text, isErr = callMCP(t, cs, "sessions_list", map[string]any{}); isErr || !strings.Contains(text, "beta") || strings.Contains(text, "alpha") {
		t.Fatalf("key of B: error %v, %s", isErr, text)
	}
	// An unknown key is unauthenticated, never an empty list.
	unknown := identity.NewAPIKey().String()
	cs = newMCPSession(t, hubFor(unknown, f.wA), false)
	if text, isErr = callMCP(t, cs, "sessions_list", map[string]any{}); !isErr || !strings.Contains(text, "unauthenticated") || strings.Contains(text, unknown) {
		t.Errorf("unknown key: error %v, %s", isErr, text)
	}
}

func TestMCPGuardedWriteThroughTheRealHubOnlyWithScopeAndOptIn(t *testing.T) {
	f := newClientFixture(t)
	target := map[string]any{"worker_id": f.wA, "session_name": "alpha"}
	hubFor := func(key string) client.Backend {
		b, err := client.NewHub(f.ch.hubConfig(t, key, ""))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	}

	// Read-only MCP (the default) has no write tool at all.
	cs := newMCPSession(t, hubFor(f.operateA), false)
	if _, _, err := cs.callTool("send_text", map[string]any{"target": target, "text": "x"}); err == nil {
		t.Error("send_text exists without the write opt-in")
	}

	// With the opt-in and an operate scope the write reaches the Worker once.
	cs = newMCPSession(t, hubFor(f.operateA), true)
	text, isErr := callMCP(t, cs, "send_text", map[string]any{"target": target, "text": "typed through the Hub"})
	if isErr || !strings.Contains(text, "delivered") {
		t.Fatalf("operate key: error %v, %s", isErr, text)
	}
	if got := f.ch.router.delivered(); len(got) != 1 || got[0] != f.wA+":alpha:typed through the Hub" {
		t.Fatalf("effects = %v", got)
	}

	// A read-only key is refused and nothing reaches the Worker.
	cs = newMCPSession(t, hubFor(f.readA), true)
	text, isErr = callMCP(t, cs, "send_text", map[string]any{"target": target, "text": "must not arrive"})
	if !isErr || !strings.Contains(text, "forbidden") {
		t.Errorf("read key: error %v, %s", isErr, text)
	}
	// A key without scope on the target Worker is refused too.
	cs = newMCPSession(t, hubFor(f.rdB), true)
	text, isErr = callMCP(t, cs, "send_text", map[string]any{"target": target, "text": "must not arrive either"})
	if !isErr || strings.Contains(text, "delivered") {
		t.Errorf("key of another Worker: error %v, %s", isErr, text)
	}
	if got := f.ch.router.delivered(); len(got) != 1 {
		t.Errorf("a denied write had an effect: %v", got)
	}
}

// TestCLIEntrypointChoosesTheRealHub runs the real `minion` binary: its main,
// its defaults, its environment. There is no local Worker for it to find, and
// it never needs one.
func TestCLIEntrypointChoosesTheRealHub(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the minion binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain to build the binary")
	}
	bin := filepath.Join(t.TempDir(), "minion")
	build := exec.Command(goBin, "build", "-o", bin, "../../../cmd/minion")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	f := newClientFixture(t)
	home := t.TempDir()
	run := func(key string, extraEnv []string, args ...string) (code int, stdout, stderr string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append([]string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_RUNTIME_DIR=" + home, "TMPDIR=" + home,
			"MINION_STATE_DIR=" + filepath.Join(home, "state"),
			"MINION_HUB_URL=" + f.ch.baseURL, "MINION_API_KEY_FILE=" + keyFile(t, key), "MINION_HUB_CA=" + f.ch.caFile,
		}, extraEnv...)
		var o, e strings.Builder
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		code = 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, o.String(), e.String()
	}

	code, out, errs := run(f.readA, nil, "sessions_list", "--hub-worker", f.wA, "--json")
	if code != 0 || !strings.Contains(out, "alpha") || strings.Contains(out, "beta") {
		t.Fatalf("sessions_list: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if strings.Contains(out+errs, f.readA) {
		t.Error("the key is in the output")
	}
	// Named by Worker id in the target, as `mn` does.
	code, out, errs = run(f.operateA, nil, "send_text", "--target", f.wA+":alpha", "--text", "hello hub")
	if code != 0 || !strings.Contains(out, "delivered") {
		t.Fatalf("send_text: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if got := f.ch.router.delivered(); len(got) != 1 || !strings.HasSuffix(got[0], ":alpha:hello hub") {
		t.Fatalf("effects = %v", got)
	}
	// Scope refuses: exit 1, the code in the message, and no effect.
	code, _, errs = run(f.readA, nil, "send_text", "--target", f.wA+":alpha", "--text", "no")
	if code != 1 || !strings.Contains(errs, "forbidden") {
		t.Errorf("read key write: exit %d, stderr %q", code, errs)
	}
	code, _, errs = run(f.readA, nil, "sessions_list", "--hub-worker", f.wB)
	if code != 1 || !strings.Contains(errs, "not_found") {
		t.Errorf("out of scope Worker: exit %d, stderr %q", code, errs)
	}
	if got := f.ch.router.delivered(); len(got) != 1 {
		t.Errorf("a denied write had an effect: %v", got)
	}
	// The key is never accepted from the plain environment, and the command
	// does not quietly fall back to a local Worker instead.
	code, _, errs = run(f.readA, []string{"MINION_API_KEY=" + f.readA}, "sessions_list", "--hub-worker", f.wA)
	if code != 1 || !strings.Contains(errs, "MINION_API_KEY_FILE") || strings.Contains(errs, f.readA) {
		t.Errorf("plain env key: exit %d, stderr %q", code, errs)
	}
	// A Worker name is not a Worker id: the real binary refuses it before it
	// sends anything (the Hub would answer invalid_argument: must be a UUIDv7),
	// and an unknown Worker id never falls back to another Worker.
	code, _, errs = run(f.readA, nil, "sessions_list", "--hub-worker", "alpha-worker")
	if code != 1 || !strings.Contains(errs, "Worker id") || strings.Contains(errs, f.readA) {
		t.Errorf("Worker name: exit %d, stderr %q", code, errs)
	}
	// A key file that others can read is refused by the binary, which names the
	// file and never the key.
	// (File modes mean nothing on Windows, whose ACL rule is the identity package's.)
	if runtime.GOOS != "windows" {
		exposed := keyFile(t, f.readA)
		if err := os.Chmod(exposed, 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errs = run(f.readA, []string{"MINION_API_KEY_FILE=" + exposed}, "sessions_list", "--hub-worker", f.wA)
		if code != 1 || !strings.Contains(errs, "API key file cannot be used") || strings.Contains(out+errs, f.readA) {
			t.Errorf("exposed key file: exit %d, stdout %q, stderr %q", code, out, errs)
		}
	}
	// A wrong CA: the certificate is refused and nothing is sent.
	other := filepath.Join(home, "other.pem")
	if err := os.WriteFile(other, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = run(f.readA, []string{"MINION_HUB_CA=" + other}, "sessions_list", "--hub-worker", f.wA)
	if code != 1 || !strings.Contains(errs, "CA file") {
		t.Errorf("bad CA: exit %d, stderr %q", code, errs)
	}
	var streamed int
	code, out, errs = run(f.readA, nil, "pipe_wait", "--target", f.wA+":alpha", "--json")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var turn ops.PipeTurn
		if json.Unmarshal([]byte(line), &turn) == nil && turn.Text == "hello" {
			streamed++
		}
	}
	if code != 0 || streamed != 2 {
		t.Errorf("pipe_wait: exit %d, %d turns, stdout %q, stderr %q", code, streamed, out, errs)
	}
}
