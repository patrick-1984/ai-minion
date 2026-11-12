package agent

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// connRun is a runtime with the core module (fake terminal host, real store)
// and the connector module (scripted dialer).
type connRun struct {
	rt     *Runtime
	host   *fakeHost
	dir    string
	dialer *scriptDialer
	logs   *logBuf
}

func startConnectorRuntime(t *testing.T, env map[string]string, prepare func(dir string, st *store.Store)) (*connRun, error) {
	t.Helper()
	dir := t.TempDir()
	if prepare != nil {
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		prepare(dir, st)
		_ = st.Close()
	}
	host := newFakeHost()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	dialer := &scriptDialer{}
	r := NewRegistry()
	if err := r.Register(ModuleCore, OrderCore, core.start); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: dialer}.start); err != nil {
		t.Fatal(err)
	}
	logs := &logBuf{}
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	rt := NewRuntime(r, Options{})
	err := rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver:   res,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err == nil {
		t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	}
	return &connRun{rt: rt, host: host, dir: dir, dialer: dialer, logs: logs}, err
}

func enrolledState(t *testing.T, hub *identity.Key, preview bool) func(string, *store.Store) {
	return func(dir string, st *store.Store) {
		e := NewEnrollments(st, dir)
		if _, err := e.LoadOrCreateKey(); err != nil {
			t.Fatal(err)
		}
		if err := e.Commit(context.Background(), Enrollment{
			Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w-1", Policy: HubPolicy{PreviewOnly: preview},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConnectorModuleIsRegisteredAfterTheCoreModule(t *testing.T) {
	names := DefaultRegistry.Names()
	core, conn := slices.Index(names, ModuleCore), slices.Index(names, ModuleConnector)
	if core < 0 || conn < 0 || conn < core {
		t.Fatalf("default modules = %v, want the connector after the core", names)
	}
}

func TestConnectorModuleWithoutAServerRunsSolo(t *testing.T) {
	c, err := startConnectorRuntime(t, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, ok := Lookup[*Connector](c.rt)
	if !ok {
		t.Fatal("no connector service")
	}
	if _, ok := Lookup[*Enrollments](c.rt); !ok {
		t.Fatal("no enrollments service")
	}
	eventually(t, func() bool { st := conn.Status(); return st.State == ConnIdle && st.Reason == "no Hub configured" })
	if c.dialer.count() != 0 {
		t.Fatal("a Solo Worker must not dial")
	}
	if !strings.Contains(c.logs.String(), "runs alone (Solo)") {
		t.Fatalf("logs = %s", c.logs.String())
	}
	if err := c.rt.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v", err)
	}
}

func TestConnectorModuleConnectsAndAnnouncesTheExecutorOps(t *testing.T) {
	hub, _ := identity.GenerateKey()
	c, err := startConnectorRuntime(t, map[string]string{"MINION_SERVER": "https://hub.example:8443"}, enrolledState(t, hub, false))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	sess := newFakeSession()
	c.dialer.script(func(p ConnectParams) (*Attempt, error) {
		mu.Lock()
		defer mu.Unlock()
		return &Attempt{
			Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: randomHash(), Version: proto.Current,
			Reply:   Reply{Type: proto.TypeWelcome, Welcome: okWelcome()},
			Session: sess,
		}, nil
	})
	conn, _ := Lookup[*Connector](c.rt)
	eventually(t, func() bool { return conn.Status().State == ConnConnected })

	p := c.dialer.call(0)
	if p.HubURL != "https://hub.example:8443" || p.Name != "w" || !bytes0(p.Pin.Pub, hub.Public()) {
		t.Fatalf("params = %+v", p)
	}
	// pipe_send is in the very first hello, with no module of the pipe in this
	// chain (Story 7.3): the executor announces it from the moment it exists.
	for _, want := range []string{ops.OpSessionsList, ops.OpSendText, ops.OpSendKeys, ops.OpAnswer, ops.OpSessionCreate, ops.OpPipeSend} {
		if !slices.Contains(p.Ops, want) {
			t.Errorf("hello ops %v miss %s", p.Ops, want)
		}
	}
	// pipe_read and pipe_wait are never executed on a Worker for the Hub.
	for _, never := range []string{ops.OpPipeRead, ops.OpPipeWait} {
		if slices.Contains(p.Ops, never) {
			t.Errorf("hello ops %v announce %s", p.Ops, never)
		}
	}
	if !slices.IsSorted(p.Ops) {
		t.Errorf("ops are not sorted: %v", p.Ops)
	}
	// The features of the same first hello: the guarded behaviours the
	// executor has itself, and no verbatim text from a host that does not
	// guarantee it (the test host does not).
	wantFeatures := []string{proto.FeatureAnswerReviewed, proto.FeaturePipeCausal, proto.FeatureSendTextSubmit}
	if !slices.Equal(p.Features, wantFeatures) {
		t.Errorf("hello features = %v, want %v", p.Features, wantFeatures)
	}

	// The local executor keeps serving while the Hub is away.
	c.host.addSession("work", "bash")
	st, _ := Lookup[*store.Store](c.rt)
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	ex, _ := Lookup[*Executor](c.rt)
	sess.drop(errors.New("connection reset"))
	eventually(t, func() bool { return conn.Status().State != ConnConnected })
	raw, err := ex.Execute(context.Background(), Request{Op: ops.OpSessionsList, Actions: []ops.Action{ops.ActionRead}})
	if err != nil || !strings.Contains(string(raw), `"work"`) {
		t.Fatalf("sessions_list while the Hub is away = %s, %v", raw, err)
	}
	// And the connector stops cleanly within the shutdown budget.
	done := make(chan error, 1)
	go func() { done <- c.rt.Stop(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}

func bytes0(a, b []byte) bool { return string(a) == string(b) }

func TestConnectorModuleAppliesTheStoredHubPolicyAtStart(t *testing.T) {
	hub, _ := identity.GenerateKey()
	c, err := startConnectorRuntime(t, map[string]string{"MINION_SERVER": "https://hub.example"}, enrolledState(t, hub, true))
	if err != nil {
		t.Fatal(err)
	}
	pol, _ := Lookup[*Policy](c.rt)
	if !pol.PreviewOnly() || !pol.HubPreviewOnly() {
		t.Fatal("the stored Hub restriction must hold before the first connection")
	}
}

func TestConnectorModuleRefusesAnInvalidServerUrl(t *testing.T) {
	_, err := startConnectorRuntime(t, map[string]string{"MINION_SERVER": "hub.example:8443"}, nil)
	if err == nil || !strings.Contains(err.Error(), `module "connector"`) || !strings.Contains(err.Error(), "server") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectorModuleNeedsTheCoreServices(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: &scriptDialer{}}.start); err != nil {
		t.Fatal(err)
	}
	err := NewRuntime(r, Options{}).Start(context.Background(), Settings{})
	if err == nil || !strings.Contains(err.Error(), "requires service") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecutorOpsListsTheRegisteredOpsSorted(t *testing.T) {
	e := newEnv(t)
	got := e.ex.Ops()
	if !slices.IsSorted(got) || !slices.Contains(got, ops.OpSessionsList) {
		t.Fatalf("Ops = %v", got)
	}
	e.ex.Handle("late_op", func(context.Context, Request) (any, error) { return nil, nil })
	if !slices.Contains(e.ex.Ops(), "late_op") {
		t.Fatal("an op registered later must be announced by the next attempt")
	}
}

// verbatimHost is a host that guarantees the verbatim paste.
type verbatimHost struct{ *fakeHost }

func (verbatimHost) VerbatimPaste() bool { return true }

// TestExecutorFeaturesFollowTheHost: the verbatim text is announced only for a
// host that says it delivers it, so that an adapter without the guarantee (an
// older or foreign host) is never advertised as safe.
func TestExecutorFeaturesFollowTheHost(t *testing.T) {
	e := newEnv(t)
	plain := e.ex.Features()
	if slices.Contains(plain, proto.FeatureTextVerbatim) || !slices.Contains(plain, proto.FeatureAnswerReviewed) {
		t.Fatalf("features of a host without the guarantee = %v", plain)
	}
	e.ex.host = verbatimHost{newFakeHost()}
	got := e.ex.Features()
	want := []string{proto.FeatureAnswerReviewed, proto.FeaturePipeCausal, proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}
	if !slices.Equal(got, want) {
		t.Fatalf("features = %v, want %v", got, want)
	}
	if !slices.IsSorted(got) {
		t.Errorf("features are not sorted: %v", got)
	}
}
