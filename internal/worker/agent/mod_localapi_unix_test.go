//go:build unix

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

func shortRunDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// providerModule registers a module that provides ex, like the core module.
func providerModule(t *testing.T, r *Registry, ex *Executor) {
	t.Helper()
	err := r.Register("fakecore", OrderCore, func(_ context.Context, env *Env) (StopFunc, error) {
		return nil, Provide(env, ex)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// startLocalWorker runs the fake core and the real local socket module with
// the socket in a private directory; it returns a client on that socket.
func startLocalWorker(t *testing.T, e *env, mut ...func(*localAPIDeps)) (*client.LocalBackend, *Runtime, string) {
	t.Helper()
	return startLocalWorkerWith(t, e, testSettings(), mut...)
}

func startLocalWorkerWith(t *testing.T, e *env, settings Settings, mut ...func(*localAPIDeps)) (*client.LocalBackend, *Runtime, string) {
	t.Helper()
	path := filepath.Join(shortRunDir(t), "minion", localapi.SocketName)
	deps := defaultLocalAPIDeps()
	deps.socketPath = func() (string, error) { return path, nil }
	for _, m := range mut {
		m(&deps)
	}
	path, _ = deps.socketPath()
	r := NewRegistry()
	providerModule(t, r, e.ex)
	if err := r.Register(ModuleLocalAPI, OrderLocalAPI, deps.start); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{StopTimeout: 5 * time.Second})
	if err := rt.Start(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return client.Local(client.WithSocketPath(path)), rt, path
}

func TestLocalAPIModuleIsRegisteredAfterTheCoreModule(t *testing.T) {
	names := DefaultRegistry.Names()
	core, local := -1, -1
	for i, n := range names {
		switch n {
		case ModuleCore:
			core = i
		case ModuleLocalAPI:
			local = i
		}
	}
	if core < 0 || local < 0 || local < core {
		t.Fatalf("default modules = %v, want %s before %s", names, ModuleCore, ModuleLocalAPI)
	}
	if OrderLocalAPI <= OrderCore {
		t.Fatalf("orders: core %d, localapi %d", OrderCore, OrderLocalAPI)
	}
}

func TestLocalAPIModuleServesTheExecutorAndStopsCleanly(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	c, rt, path := startLocalWorker(t, e)

	raw, err := c.Call(context.Background(), ops.OpSessionsList, nil)
	if err != nil {
		t.Fatal(err)
	}
	var list ops.SessionsListResult
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Sessions) != 1 || list.Sessions[0].Name != "work" {
		t.Fatalf("sessions_list = %s (%v)", raw, err)
	}

	if err := rt.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file after stop: %v", err)
	}
	if _, err := c.Call(context.Background(), ops.OpSessionsList, nil); !errors.Is(err, client.ErrWorkerNotRunning) {
		t.Fatalf("Call after stop = %v", err)
	}
}

func TestLocalCallerHasFullActionsIncludingWrites(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "claude", "idle")
	c, _, _ := startLocalWorker(t, e)

	raw, err := c.Call(context.Background(), ops.OpSendText, sendText("work", "hello", false))
	if err != nil {
		t.Fatal(err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text = %s (%v)", raw, err)
	}
	if got := e.host.pastes(); len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("pastes = %+v", got)
	}
}

func TestAPIErrorsKeepCodeAndReasonOverTheSocket(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.policy.SetPreviewOnly(true)
	c, _, _ := startLocalWorker(t, e)

	_, err := c.Call(context.Background(), ops.OpSendText, sendText("work", "x", false))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeForbidden {
		t.Fatalf("err = %v, want forbidden", err)
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(pe.Details, &d) != nil || d.Reason != string(proto.CodePreviewOnly) {
		t.Fatalf("details = %s, want reason preview_only", pe.Details)
	}

	_, err = c.Call(context.Background(), ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: target("missing")})
	if proto.CodeOf(err) != proto.CodeTargetNotFound {
		t.Fatalf("missing session: %v", err)
	}
	_, err = c.Call(context.Background(), "no_such_op", nil)
	if proto.CodeOf(err) != proto.CodeUnsupportedOp {
		t.Fatalf("unknown op: %v", err)
	}
	_, err = c.Call(context.Background(), ops.OpSessionSnapshot, json.RawMessage(`{"target":`))
	if proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("bad args: %v", err)
	}
}

func TestStreamingHandlerEmitsProgressOverTheSocket(t *testing.T) {
	e := newEnv(t)
	// A streaming op of the executor; the pipe ops are served by the pipe
	// service, not by the executor (Story 7.3).
	const streamOp = "stream_op"
	e.ex.Handle(streamOp, func(ctx context.Context, rq Request) (any, error) {
		for i := 1; i <= 3; i++ {
			if err := EmitProgress(ctx, map[string]int{"turn": i}); err != nil {
				return nil, err
			}
		}
		return struct{}{}, nil
	})
	c, _, _ := startLocalWorker(t, e)

	var got []string
	res, err := c.Stream(context.Background(), streamOp, nil, func(m json.RawMessage) error {
		got = append(got, string(m))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"turn":1}`, `{"turn":2}`, `{"turn":3}`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
	if string(res) != `{}` {
		t.Fatalf("result = %s", res)
	}
}

func TestEmitProgressWithoutASinkIsANoOp(t *testing.T) {
	if err := EmitProgress(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := EmitProgress(WithProgress(context.Background(), nil), 1); err != nil {
		t.Fatal(err)
	}
	var got []string
	ctx := WithProgress(context.Background(), func(m json.RawMessage) { got = append(got, string(m)) })
	if err := EmitProgress(ctx, "a"); err != nil || !reflect.DeepEqual(got, []string{`"a"`}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if err := EmitProgress(ctx, make(chan int)); err == nil {
		t.Fatal("an unencodable value was accepted")
	}
}

func TestAPIErrorProtoErrorOmitsTheCause(t *testing.T) {
	ae := &APIError{Code: proto.CodeInternal, Message: "x failed", Err: errors.New("secret /path")}
	pe := ae.ProtoError()
	b, _ := json.Marshal(pe.Frame())
	if strings.Contains(string(b), "secret") || pe.Code != proto.CodeInternal || pe.Details != nil {
		t.Fatalf("frame = %s", b)
	}
}

func TestLocalAPIModuleNeedsTheCoreExecutor(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(ModuleLocalAPI, OrderLocalAPI, defaultLocalAPIDeps().start); err != nil {
		t.Fatal(err)
	}
	err := NewRuntime(r, Options{}).Start(context.Background(), testSettings())
	if err == nil || !strings.Contains(err.Error(), "requires service") {
		t.Fatalf("err = %v", err)
	}
}

func TestLocalAPIModuleRefusesToTakeOverALiveSocket(t *testing.T) {
	e := newEnv(t)
	_, _, path := startLocalWorker(t, e)

	r := NewRegistry()
	providerModule(t, r, e.ex)
	deps := defaultLocalAPIDeps()
	deps.socketPath = func() (string, error) { return path, nil }
	if err := r.Register(ModuleLocalAPI, OrderLocalAPI, deps.start); err != nil {
		t.Fatal(err)
	}
	err := NewRuntime(r, Options{}).Start(context.Background(), testSettings())
	if !errors.Is(err, localapi.ErrAlreadyRunning) {
		t.Fatalf("second Worker start = %v, want ErrAlreadyRunning", err)
	}
}

func TestLocalAPIModuleReplacesAStaleSocket(t *testing.T) {
	path := filepath.Join(shortRunDir(t), "minion", localapi.SocketName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()

	e := newEnv(t)
	e.host.addSession("work", "claude")
	c, _, _ := startLocalWorker(t, e, func(d *localAPIDeps) { d.socketPath = func() (string, error) { return path, nil } })
	if _, err := c.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLocalAPIModuleWithoutUnixSocketsLetsTheWorkerRun(t *testing.T) {
	e := newEnv(t)
	r := NewRegistry()
	providerModule(t, r, e.ex)
	deps := localAPIDeps{
		socketPath: func() (string, error) { return "/x/worker.sock", nil },
		listen:     func(string) (net.Listener, error) { return nil, localapi.ErrUnsupported },
	}
	if err := r.Register(ModuleLocalAPI, OrderLocalAPI, deps.start); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), testSettings()); err != nil {
		t.Fatalf("Start = %v, want nil: the Worker runs without a local socket", err)
	}
	if err := rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLocalAPIModuleReportsSocketPathAndListenErrors(t *testing.T) {
	e := newEnv(t)
	for name, deps := range map[string]localAPIDeps{
		"path":   {socketPath: func() (string, error) { return "", errors.New("no home") }, listen: localapi.Listen},
		"listen": {socketPath: func() (string, error) { return "/x", nil }, listen: func(string) (net.Listener, error) { return nil, errors.New("denied") }},
	} {
		r := NewRegistry()
		providerModule(t, r, e.ex)
		if err := r.Register(ModuleLocalAPI, OrderLocalAPI, deps.start); err != nil {
			t.Fatal(err)
		}
		if err := NewRuntime(r, Options{}).Start(context.Background(), testSettings()); err == nil {
			t.Fatalf("%s: Start succeeded", name)
		}
	}
}

// TestLocalClientDrivesARealTmuxServer is the integration test of Story 2.8:
// the Worker runtime (core and local socket modules) on a private tmux
// server, and the local client calling sessions_list and send_text.
func TestLocalClientDrivesARealTmuxServer(t *testing.T) {
	srv := tmuxtest.New(t)
	target := srv.NewSession("catwork", "cat")

	run := shortRunDir(t)
	// The default socket path of module and client: XDG_RUNTIME_DIR is honoured
	// on Linux only, TMPDIR decides everywhere else, so set both.
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("TMPDIR", run)
	sock, err := localapi.SocketPath(config.SystemPathEnv())
	if err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	if err := RegisterCore(r); err != nil {
		t.Fatal(err)
	}
	if err := RegisterLocalAPI(r); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{StopTimeout: 10 * time.Second})
	settings := Settings{WorkerCore: config.WorkerCore{
		StateDir: t.TempDir(), TmuxSocket: srv.Socket(), Name: "it", ShareExisting: config.ShareExistingAll,
	}}
	if err := rt.Start(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rt.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	if fi, err := os.Lstat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %s: %v %v", sock, fi, err)
	}

	c := client.Local() // default lookup: the path computed above
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := c.Call(ctx, ops.OpSessionsList, nil)
	if err != nil {
		t.Fatal(err)
	}
	var list ops.SessionsListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list.Sessions {
		found = found || s.Name == "catwork"
	}
	if !found {
		t.Fatalf("sessions_list = %s, want catwork", raw)
	}

	raw, err = c.Call(ctx, ops.OpSendText, sendText("catwork", "hello-over-the-socket", false))
	if err != nil {
		t.Fatal(err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text = %s (%v)", raw, err)
	}
	srv.WaitFor(target, "hello-over-the-socket", 10*time.Second)
}

// syncBuffer is a bytes.Buffer that the log handler and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// An internal failure leaves a trace in the Worker log (op, code and cause)
// while the caller gets the code and a generic message, never the cause.
func TestInternalErrorsAreLoggedWithTheirCauseButNotSentToTheClient(t *testing.T) {
	const cause = "store exploded: disk-detail-7f3a"
	e := newEnv(t)
	e.ex.Handle(ops.OpSessionsList, func(context.Context, Request) (any, error) {
		return nil, internalErr("read the share selection", errors.New(cause))
	})
	var logs syncBuffer
	settings := testSettings()
	settings.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, _, _ := startLocalWorkerWith(t, e, settings)

	_, err := c.Call(context.Background(), ops.OpSessionsList, map[string]string{"secret_arg": "arg-value-9c1d"})
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeInternal {
		t.Fatalf("err = %v, want an internal error", err)
	}
	if strings.Contains(err.Error(), "disk-detail") || strings.Contains(string(pe.Details), "disk-detail") {
		t.Fatalf("the cause reached the client: %v", err)
	}

	out := logs.String()
	for _, want := range []string{"level=ERROR", "op=sessions_list", "code=internal", "disk-detail-7f3a"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "arg-value-9c1d") {
		t.Errorf("the log contains the arguments of the request:\n%s", out)
	}
}

// Errors that are not internal are the caller's business and are not logged
// at error level.
func TestNonInternalErrorsAreNotLoggedAtErrorLevel(t *testing.T) {
	e := newEnv(t)
	var logs syncBuffer
	settings := testSettings()
	settings.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, _, _ := startLocalWorkerWith(t, e, settings)

	if _, err := c.Call(context.Background(), ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: target("missing")}); proto.CodeOf(err) != proto.CodeTargetNotFound {
		t.Fatalf("err = %v", err)
	}
	if out := logs.String(); strings.Contains(out, "level=ERROR") {
		t.Fatalf("a caller error was logged as an error:\n%s", out)
	}
}
