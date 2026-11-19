package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func createArgs(name string) ops.SessionCreateArgs {
	return ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: name}
}

// Blocked: session_create is refused for every origin with
// forbidden/new_sessions_blocked; existing sessions and other writes work.
func TestNewSessionsBlockedRefusesEveryOrigin(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.ex.SetNewSessionsBlocked(true, []string{"file"})
	for name, origin := range map[string]*proto.Origin{
		"local socket, mn and MCP (no origin)": nil,
		"Hub user":                             {Kind: proto.OriginUser, UserID: "u1"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := mustJSON(t, createArgs("fresh"))
			_, err := e.ex.Execute(context.Background(), Request{Op: ops.OpSessionCreate, Actions: allActions, Args: raw, Origin: origin})
			wantAPIError(t, err, "forbidden", "new_sessions_blocked")
		})
	}
	if len(e.host.sessions) != 1 {
		t.Fatal("a blocked Worker created a session")
	}
	if res := e.mustWrite(ops.OpSendText, sendText("work", "x", false)); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text under the block: %+v", res)
	}
	// The update's hold release and the Hub's policy do not lift it.
	e.ex.SetSessionCreateHold("")
	e.policy.SetHubPreviewOnly(false)
	_, err := e.write(ops.OpSessionCreate, createArgs("fresh2"))
	wantAPIError(t, err, "forbidden", "new_sessions_blocked")
	if blocked, src := e.policy.NewSessionsBlocked(); !blocked || len(src) != 1 || src[0] != "file" {
		t.Fatalf("NewSessionsBlocked = %t %v", blocked, src)
	}
	// Unblocked again: it works.
	e.ex.SetNewSessionsBlocked(false, nil)
	if res := e.mustWrite(ops.OpSessionCreate, createArgs("fresh3")); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("after the unblock: %+v", res)
	}
}

// The setter waits for a creation in flight: after it returns, no creation
// that started before can still finish unseen.
func TestNewSessionsBlockWaitsForCreateInFlight(t *testing.T) {
	e := &Executor{policy: NewPolicy(nil)}
	e.createMu.RLock() // a session_create in flight
	set := make(chan struct{})
	go func() { e.SetNewSessionsBlocked(true, []string{"env"}); close(set) }()
	select {
	case <-set:
		t.Fatal("the block did not wait for the creation in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if blocked, _ := e.policy.NewSessionsBlocked(); blocked {
		t.Fatal("blocked before the creation in flight ended")
	}
	e.createMu.RUnlock()
	select {
	case <-set:
	case <-time.After(5 * time.Second):
		t.Fatal("the block never took effect")
	}
}

// A service started without the installer's environment reads the block
// from the config file: a real session_create is refused.
func TestServiceStartWithoutInstallerEnvRefusesCreate(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.LoadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set(config.KeyAllowNewSessions, config.AllowNewSessionsOff)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// The service's environment says nothing (or on): the file still blocks.
	res := &config.Resolver{Role: config.ScopeWorker, File: cfg, LookupEnv: func(k string) (string, bool) {
		switch k {
		case "MINION_STATE_DIR":
			return dir, true
		case "MINION_TMUX_SOCKET":
			return filepath.Join(dir, "tmux.sock"), true
		case "MINION_ALLOW_NEW_SESSIONS":
			return "on", true
		}
		return "", false
	}}
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	cd := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	r := NewRegistry()
	if err := r.Register(ModuleCore, OrderCore, cd.start); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), Settings{WorkerCore: core, Resolver: res, Logger: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	ex, _ := Lookup[*Executor](rt)
	_, err = ex.Execute(context.Background(), Request{Op: ops.OpSessionCreate, Actions: allActions, Args: mustJSON(t, createArgs("fresh"))})
	wantAPIError(t, err, "forbidden", "new_sessions_blocked")
	if len(host.sessions) != 0 {
		t.Fatal("a session was created")
	}
}

// startCoreWith starts the lifecycle and core modules with s.
func startCoreWith(t *testing.T, s Settings) (*Runtime, *fakeHost) {
	t.Helper()
	host := newFakeHost()
	cd := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	r := NewRegistry()
	if err := RegisterLifecycle(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ModuleCore, OrderCore, cd.start); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return rt, host
}

func saveAllowNewSessions(t *testing.T, dir, v string) {
	t.Helper()
	cfg, err := config.LoadFile(filepath.Join(dir, WorkerConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set(config.KeyAllowNewSessions, v)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// The Worker loaded its config before a command saved off, and took the
// lifecycle lock after: it rereads the block under the lock and refuses
// creates from its first module on.
func TestStartupRereadsBlockUnderLock(t *testing.T) {
	dir := t.TempDir()
	res := &config.Resolver{Role: config.ScopeWorker, File: config.MapSource{}, LookupEnv: func(string) (string, bool) { return "", false }}
	s := Settings{WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver: res, Logger: slog.New(slog.DiscardHandler)}
	if s.NewSessions.Blocked {
		t.Fatal("the stale snapshot must allow")
	}
	// The barrier: between the config load and the lock, the CLI saves off.
	saveAllowNewSessions(t, dir, "off")
	rt, host := startCoreWith(t, s)
	ex, _ := Lookup[*Executor](rt)
	_, err := ex.Execute(context.Background(), Request{Op: ops.OpSessionCreate, Actions: allActions, Args: mustJSON(t, createArgs("fresh"))})
	wantAPIError(t, err, "forbidden", "new_sessions_blocked")
	if len(host.sessions) != 0 {
		t.Fatal("a session was created")
	}
}

// The live apply rereads the file from disk with the Worker's own flag and
// environment snapshot; the resolver's cached File is never consulted.
func TestLiveApplyIgnoresCachedResolver(t *testing.T) {
	dir := t.TempDir()
	cached := config.MapSource{config.KeyAllowNewSessions: "on"}
	res := &config.Resolver{Role: config.ScopeWorker, File: cached, LookupEnv: func(string) (string, bool) { return "", false }}
	s := Settings{WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver: res, Logger: slog.New(slog.DiscardHandler)}
	rt, _ := startCoreWith(t, s)
	ex, _ := Lookup[*Executor](rt)
	a := configApplier{settings: s, ex: ex}
	saveAllowNewSessions(t, dir, "off")
	got, err := a.apply(config.KeyAllowNewSessions)
	if err != nil || got.Effective != "off" || len(got.Sources) != 1 || got.Sources[0] != "file" {
		t.Fatalf("apply = %+v, %v", got, err)
	}
	if blocked, _ := ex.Policy().NewSessionsBlocked(); !blocked {
		t.Fatal("not blocked after the apply")
	}
	saveAllowNewSessions(t, dir, "on")
	if got, _ := a.apply(config.KeyAllowNewSessions); got.Effective != "on" {
		t.Fatalf("after on: %+v", got)
	}
	// The Worker's environment snapshot still restricts.
	s.Resolver = &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { return "off", k == "MINION_ALLOW_NEW_SESSIONS" }}
	a.settings = s
	if got, _ := a.apply(config.KeyAllowNewSessions); got.Effective != "off" || got.Sources[0] != "env" {
		t.Fatalf("env off: %+v", got)
	}
	// Other keys are refused by the op.
	if _, err := a.apply(config.KeyPreviewOnly); err == nil || !strings.Contains(err.Error(), "restart the Worker") {
		t.Fatalf("preview_only = %v", err)
	}
}

// Gate 1: an apply that waits for a creation in flight rereads the file only
// once it holds the create lock, so that a later save of "off" is never
// undone by the stale "on" it would have read before the wait.
func TestLiveApplyRereadsAfterWaitingForCreates(t *testing.T) {
	dir := t.TempDir()
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(string) (string, bool) { return "", false }}
	s := Settings{WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver: res, Logger: slog.New(slog.DiscardHandler)}
	rt, _ := startCoreWith(t, s)
	ex, _ := Lookup[*Executor](rt)
	a := configApplier{settings: s, ex: ex}
	saveAllowNewSessions(t, dir, "on")
	ex.createMu.RLock() // a creation in flight
	done := make(chan ConfigApplyResult)
	go func() {
		got, _ := a.apply(config.KeyAllowNewSessions)
		done <- got
	}()
	time.Sleep(50 * time.Millisecond) // the apply waits for the creation
	saveAllowNewSessions(t, dir, "off")
	ex.createMu.RUnlock()
	got := <-done
	if blocked, _ := ex.Policy().NewSessionsBlocked(); !blocked || got.Effective != "off" {
		t.Fatalf("after the later save of off: blocked=%v, apply=%+v", blocked, got)
	}
}
