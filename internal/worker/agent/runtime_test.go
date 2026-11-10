package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// eventLog records what the fake modules do, in order.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, e)
}

func (l *eventLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

// fakeModule registers a module that logs its start and stop.
func fakeModule(t *testing.T, r *Registry, l *eventLog, name string, order int) {
	t.Helper()
	err := r.Register(name, order, func(context.Context, *Env) (StopFunc, error) {
		l.add("start " + name)
		return func(context.Context) error { l.add("stop " + name); return nil }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testSettings() Settings {
	return Settings{WorkerCore: config.WorkerCore{TmuxSocket: "/tmp/x/tmux.sock", Name: "w"}}
}

func TestModulesStartInOrderAndStopInReverse(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	// Registered out of order: the order number decides, ties go by name.
	fakeModule(t, r, l, "c", 20)
	fakeModule(t, r, l, "b", 10)
	fakeModule(t, r, l, "a", 10)
	fakeModule(t, r, l, "d", 30)
	if got, want := r.Names(), []string{"a", "b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}

	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), testSettings()); err != nil {
		t.Fatal(err)
	}
	if got, want := rt.Modules(), []string{"a", "b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Modules = %v, want %v", got, want)
	}
	if err := rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := rt.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop = %v, want nil (idempotent)", err)
	}
	want := []string{"start a", "start b", "start c", "start d", "stop d", "stop c", "stop b", "stop a"}
	if got := l.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if err := rt.Start(context.Background(), testSettings()); err == nil {
		t.Fatal("a runtime must start only once")
	}
}

func TestFailedStartStopsStartedModulesInReverse(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "a", 1)
	fakeModule(t, r, l, "b", 2)
	boom := errors.New("boom")
	if err := r.Register("c", 3, func(context.Context, *Env) (StopFunc, error) { l.add("start c"); return nil, boom }); err != nil {
		t.Fatal(err)
	}
	fakeModule(t, r, l, "d", 4)

	err := NewRuntime(r, Options{}).Start(context.Background(), testSettings())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `module "c"`) {
		t.Fatalf("err = %v, want it to wrap boom and name module c", err)
	}
	want := []string{"start a", "start b", "start c", "stop b", "stop a"}
	if got := l.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v (d never starts, the failed c has nothing to stop)", got, want)
	}
}

func TestPanickingModuleIsAStartError(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("p", 1, func(context.Context, *Env) (StopFunc, error) { panic("oops") }); err != nil {
		t.Fatal(err)
	}
	err := NewRuntime(r, Options{}).Start(context.Background(), testSettings())
	if err == nil || !strings.Contains(err.Error(), "panic: oops") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopContinuesAfterFailureAndIsBounded(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "a", 1)
	release := make(chan struct{})
	defer close(release)
	if err := r.Register("slow", 2, func(context.Context, *Env) (StopFunc, error) {
		return func(context.Context) error { <-release; return nil }, nil // ignores its context
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("bad", 3, func(context.Context, *Env) (StopFunc, error) {
		return func(context.Context) error { return errors.New("close failed") }, nil
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewRuntime(r, Options{StopTimeout: 100 * time.Millisecond})
	if err := rt.Start(context.Background(), testSettings()); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	err := rt.Stop(context.Background())
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("Stop took %v with a 100ms timeout", took)
	}
	if err == nil || !strings.Contains(err.Error(), `"bad"`) || !strings.Contains(err.Error(), "close failed") || !strings.Contains(err.Error(), `"slow"`) {
		t.Fatalf("err = %v, want the failures of bad and slow", err)
	}
	if got := l.get(); got[len(got)-1] != "stop a" {
		t.Fatalf("events = %v, want module a stopped last despite the failures", got)
	}
}

func TestStopOvershootIsIndependentOfTheNumberOfBlockedModules(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "first", 1)
	release := make(chan struct{})
	defer close(release)
	blocked := []string{"blocked-a", "blocked-b", "blocked-c", "blocked-d"}
	for i, name := range blocked {
		if err := r.Register(name, 2+i, func(context.Context, *Env) (StopFunc, error) {
			return func(context.Context) error { <-release; return nil }, nil // ignores its context
		}); err != nil {
			t.Fatal(err)
		}
	}
	const timeout = 300 * time.Millisecond
	rt := NewRuntime(r, Options{StopTimeout: timeout})
	if err := rt.Start(context.Background(), testSettings()); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	err := rt.Stop(context.Background())
	if took := time.Since(began); took > timeout+100*time.Millisecond {
		t.Fatalf("Stop took %v with a %v timeout, want at most %v", took, timeout, timeout+100*time.Millisecond)
	}
	if err == nil {
		t.Fatal("Stop returned nil, want the abandoned modules named")
	}
	for _, name := range blocked {
		if !strings.Contains(err.Error(), `"`+name+`"`) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
	if got := l.get(); got[len(got)-1] != "stop first" {
		t.Fatalf("events = %v, want the first module stopped last", got)
	}
}

func TestDefaultStopTimeoutIsTenSeconds(t *testing.T) {
	if DefaultStopTimeout != 10*time.Second {
		t.Fatalf("DefaultStopTimeout = %v", DefaultStopTimeout)
	}
	if rt := NewRuntime(NewRegistry(), Options{}); rt.opts.StopTimeout != 10*time.Second {
		t.Fatalf("timeout = %v", rt.opts.StopTimeout)
	}
}

func TestRegistryRejectsMisuse(t *testing.T) {
	r := NewRegistry()
	ok := func(context.Context, *Env) (StopFunc, error) { return nil, nil }
	if err := r.Register("", 1, ok); err == nil {
		t.Error("empty name accepted")
	}
	if err := r.Register("x", 1, nil); err == nil {
		t.Error("nil start accepted")
	}
	if err := r.Register("x", 1, ok); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("x", 2, ok); err == nil {
		t.Error("duplicate name accepted")
	}
	if err := r.RegisterPreStart("", func(context.Context, Settings) error { return nil }); err == nil {
		t.Error("empty hook name accepted")
	}
	if err := r.RegisterPreStart("h", nil); err == nil {
		t.Error("nil hook accepted")
	}
	if err := r.RegisterPreStart("h", func(context.Context, Settings) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterPreStart("h", func(context.Context, Settings) error { return nil }); err == nil {
		t.Error("duplicate hook accepted")
	}
}

func TestRegisterModuleAddsToDefaultRegistryAndPanicsOnMisuse(t *testing.T) {
	saved := DefaultRegistry
	DefaultRegistry = NewRegistry()
	defer func() { DefaultRegistry = saved }()

	// A mod_<name>.go file does exactly this from its init function.
	RegisterModule("mod-a", 5, func(context.Context, *Env) (StopFunc, error) { return nil, nil })
	RegisterPreStart("hook-a", func(context.Context, Settings) error { return nil })
	if got := DefaultRegistry.Names(); !reflect.DeepEqual(got, []string{"mod-a"}) {
		t.Fatalf("Names = %v", got)
	}
	if got := DefaultRegistry.PreStartNames(); !reflect.DeepEqual(got, []string{"hook-a"}) {
		t.Fatalf("PreStartNames = %v", got)
	}
	for name, fn := range map[string]func(){
		"duplicate module": func() { RegisterModule("mod-a", 1, func(context.Context, *Env) (StopFunc, error) { return nil, nil }) },
		"duplicate hook":   func() { RegisterPreStart("hook-a", func(context.Context, Settings) error { return nil }) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestPreStartHooksRunOnceInOrderBeforeModules(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "m", 1)
	for _, n := range []string{"enroll", "migrate", "announce"} {
		if err := r.RegisterPreStart(n, func(_ context.Context, s Settings) error {
			if s.TmuxSocket == "" {
				t.Error("a hook must see the resolved settings")
			}
			l.add("hook " + n)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), testSettings()); err != nil {
		t.Fatal(err)
	}
	_ = rt.Stop(context.Background())
	want := []string{"hook enroll", "hook migrate", "hook announce", "start m", "stop m"}
	if got := l.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRunPreStartAloneForWorkerUp(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "m", 1)
	if err := r.RegisterPreStart("enroll", func(_ context.Context, s Settings) error {
		l.add("hook enroll")
		_, _ = s.Out.Write([]byte("enrolled\n"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := testSettings()
	s.Out = &out
	if err := r.RunPreStart(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := l.get(); !reflect.DeepEqual(got, []string{"hook enroll"}) {
		t.Fatalf("events = %v: RunPreStart must not start modules", got)
	}
	if out.String() != "enrolled\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestFailingPreStartHookAbortsBeforeAnyModule(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "m", 1)
	boom := errors.New("invitation expired")
	if err := r.RegisterPreStart("enroll", func(context.Context, Settings) error { return boom }); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterPreStart("after", func(context.Context, Settings) error { l.add("hook after"); return nil }); err != nil {
		t.Fatal(err)
	}
	err := NewRuntime(r, Options{}).Start(context.Background(), testSettings())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `"enroll"`) {
		t.Fatalf("err = %v", err)
	}
	if got := l.get(); len(got) != 0 {
		t.Fatalf("events = %v, want nothing after a failed hook", got)
	}
}

type greeter interface{ Greet() string }
type english struct{}

func (english) Greet() string { return "hello" }

func TestServicesAreProvidedToLaterModules(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("provider", 1, func(_ context.Context, env *Env) (StopFunc, error) {
		return nil, Provide[greeter](env, english{})
	}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := r.Register("consumer", 2, func(_ context.Context, env *Env) (StopFunc, error) {
		g, err := Require[greeter](env)
		if err != nil {
			return nil, err
		}
		got = g.Greet() + " from " + env.Module()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), testSettings()); err != nil {
		t.Fatal(err)
	}
	if got != "hello from consumer" {
		t.Fatalf("got %q", got)
	}
	if g, ok := Lookup[greeter](rt); !ok || g.Greet() != "hello" {
		t.Fatalf("Lookup = %v, %v", g, ok)
	}
	if _, ok := Lookup[*Executor](rt); ok {
		t.Fatal("Lookup found a service nobody provided")
	}
}

func TestProvideAndRequireErrors(t *testing.T) {
	r := NewRegistry()
	_ = r.Register("a", 1, func(_ context.Context, env *Env) (StopFunc, error) {
		if err := Provide[greeter](env, english{}); err != nil {
			return nil, err
		}
		if err := Provide[greeter](env, english{}); err == nil || !strings.Contains(err.Error(), "already provides") {
			t.Errorf("second provider: %v", err)
		}
		if err := Provide[greeter](env, nil); err == nil {
			t.Error("nil service accepted")
		}
		return nil, nil
	})
	_ = r.Register("early", 0, func(_ context.Context, env *Env) (StopFunc, error) {
		_, err := Require[greeter](env)
		return nil, err
	})
	err := NewRuntime(r, Options{}).Start(context.Background(), testSettings())
	if err == nil || !strings.Contains(err.Error(), `"early"`) || !strings.Contains(err.Error(), "lower order") {
		t.Fatalf("err = %v, want the early module named", err)
	}
}

func TestEnvExposesSettings(t *testing.T) {
	r := NewRegistry()
	var seen Settings
	_ = r.Register("probe", 1, func(_ context.Context, env *Env) (StopFunc, error) {
		seen = env.Settings()
		return nil, nil
	})
	s := testSettings()
	s.Logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if err := NewRuntime(r, Options{}).Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if seen.TmuxSocket != "/tmp/x/tmux.sock" || seen.Name != "w" || seen.Logger == nil {
		t.Fatalf("settings = %+v", seen)
	}
}

func TestRunStopsWhenContextEnds(t *testing.T) {
	l := &eventLog{}
	r := NewRegistry()
	fakeModule(t, r, l, "a", 1)
	fakeModule(t, r, l, "b", 2)
	rt := NewRuntime(r, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx, testSettings()) }()
	waitFor(t, func() bool { return len(l.get()) == 2 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
	want := []string{"start a", "start b", "stop b", "stop a"}
	if got := l.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRunReturnsStartFailure(t *testing.T) {
	r := NewRegistry()
	boom := errors.New("boom")
	_ = r.Register("x", 1, func(context.Context, *Env) (StopFunc, error) { return nil, boom })
	if err := NewRuntime(r, Options{}).Run(context.Background(), testSettings()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- core module -----------------------------------------------------------

type coreRun struct {
	rt       *Runtime
	host     *fakeHost
	stateDir string
	socket   string
}

// startCore starts only the core module with a fake host and a real store.
func startCore(t *testing.T, stateDir string, wc config.WorkerCore) *coreRun {
	t.Helper()
	host := newFakeHost()
	var gotSocket string
	deps := coreDeps{
		openStore:  func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
		newHost:    func(socket string) (term.SessionHost, error) { gotSocket = socket; return host, nil },
		classifier: newFakeClassifier(),
	}
	r := NewRegistry()
	if err := r.Register(ModuleCore, OrderCore, deps.start); err != nil {
		t.Fatal(err)
	}
	wc.StateDir = stateDir
	if wc.TmuxSocket == "" {
		wc.TmuxSocket = filepath.Join(t.TempDir(), "tmux.sock")
	}
	if wc.Name == "" {
		wc.Name = "w"
	}
	rt := NewRuntime(r, Options{})
	if err := rt.Start(context.Background(), Settings{WorkerCore: wc}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return &coreRun{rt: rt, host: host, stateDir: stateDir, socket: gotSocket}
}

func TestCoreModuleProvidesServicesAndPassesTheSocketOnce(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	c := startCore(t, t.TempDir(), config.WorkerCore{TmuxSocket: sock})
	if c.socket != sock {
		t.Fatalf("host got socket %q, want the resolved %q", c.socket, sock)
	}
	if _, ok := Lookup[*store.Store](c.rt); !ok {
		t.Error("no store service")
	}
	if _, ok := Lookup[term.SessionHost](c.rt); !ok {
		t.Error("no host service")
	}
	if _, ok := Lookup[*Policy](c.rt); !ok {
		t.Error("no policy service")
	}
	ex, ok := Lookup[*Executor](c.rt)
	if !ok {
		t.Fatal("no executor service")
	}
	// The executor runs ops against the host it was built with.
	c.host.addSession("work", "bash")
	st, _ := Lookup[*store.Store](c.rt)
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	raw, err := ex.Execute(context.Background(), Request{Op: ops.OpSessionsList, Actions: []ops.Action{ops.ActionRead}})
	if err != nil || !strings.Contains(string(raw), `"work"`) {
		t.Fatalf("sessions_list = %s, %v", raw, err)
	}
}

func TestCoreModuleAppliesSettingsToStoreAndPolicy(t *testing.T) {
	dir := t.TempDir()
	c := startCore(t, dir, config.WorkerCore{ShareExisting: config.ShareExistingAll, PreviewOnly: true})
	st, _ := Lookup[*store.Store](c.rt)
	sel, err := st.LoadShare()
	if err != nil || sel.Mode != store.ShareAll || !sel.Configured {
		t.Fatalf("share = %+v, %v", sel, err)
	}
	pol, _ := Lookup[*Policy](c.rt)
	if !pol.PreviewOnly() {
		t.Fatal("preview_only was not applied to the policy")
	}
	ok, err := pol.Shared(context.Background(), "anything")
	if err != nil || !ok {
		t.Fatalf("Shared = %v, %v", ok, err)
	}
	// A write is refused by the policy, whatever actions the caller holds.
	c.host.addSession("work", "bash")
	ex, _ := Lookup[*Executor](c.rt)
	_, err = ex.Execute(context.Background(), Request{Op: ops.OpSendText, Actions: []ops.Action{ops.ActionOperate},
		Args: mustJSON(t, sendText("work", "x", false))})
	wantAPIError(t, err, "forbidden", "preview_only")
}

func TestCoreModuleKeepsTheSavedShareWhenShareExistingIsUnset(t *testing.T) {
	dir := t.TempDir()
	first := startCore(t, dir, config.WorkerCore{ShareExisting: config.ShareExistingNone})
	st, _ := Lookup[*store.Store](first.rt)
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := first.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A restart without the setting leaves the choice made in the meantime.
	second := startCore(t, dir, config.WorkerCore{})
	st2, _ := Lookup[*store.Store](second.rt)
	sel, err := st2.LoadShare()
	if err != nil || sel.Mode != store.ShareSelect || len(sel.Sessions) != 2 {
		t.Fatalf("share = %+v, %v", sel, err)
	}
	pol, _ := Lookup[*Policy](second.rt)
	if pol.PreviewOnly() {
		t.Fatal("preview-only must be off by default")
	}

	// An explicit value replaces the mode and keeps the selected names.
	if err := second.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	third := startCore(t, dir, config.WorkerCore{ShareExisting: config.ShareExistingLater})
	st3, _ := Lookup[*store.Store](third.rt)
	sel, _ = st3.LoadShare()
	if sel.Mode != store.ShareLater || len(sel.Sessions) != 2 {
		t.Fatalf("share = %+v", sel)
	}
}

func TestCoreModuleStopClosesTheStore(t *testing.T) {
	c := startCore(t, t.TempDir(), config.WorkerCore{})
	st, _ := Lookup[*store.Store](c.rt)
	if err := c.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadShare(); err == nil {
		t.Fatal("the store is still open after Stop")
	}
}

func TestCoreModuleStopClosesTheTerminalHost(t *testing.T) {
	c := startCore(t, t.TempDir(), config.WorkerCore{})
	if err := c.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.host.mu.Lock()
	defer c.host.mu.Unlock()
	if c.host.closes != 1 {
		t.Fatalf("host closed %d times, want once", c.host.closes)
	}
}

func TestCoreModuleStartFailureAfterTheHostClosesTheHost(t *testing.T) {
	host := newFakeHost()
	deps := coreDeps{
		openStore:  func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: newFakeClassifier(),
	}
	r := NewRegistry()
	_ = r.Register(ModuleCore, OrderCore, deps.start)
	err := NewRuntime(r, Options{}).Start(context.Background(),
		Settings{WorkerCore: config.WorkerCore{StateDir: t.TempDir(), TmuxSocket: "/tmp/x/s", Name: "w", ShareExisting: "bogus"}})
	if err == nil || !strings.Contains(err.Error(), "share_existing") {
		t.Fatalf("err = %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.closes != 1 {
		t.Fatalf("host closed %d times after a failed start, want once", host.closes)
	}
}

func TestCoreModuleFailureClosesTheStoreAndNamesTheStep(t *testing.T) {
	hostErr := errors.New("tmux is too old")
	var opened *store.Store
	deps := coreDeps{
		openStore: func(ctx context.Context, dir string) (*store.Store, error) {
			st, err := store.OpenContext(ctx, dir)
			opened = st
			return st, err
		},
		newHost:    func(string) (term.SessionHost, error) { return nil, hostErr },
		classifier: newFakeClassifier(),
	}
	r := NewRegistry()
	_ = r.Register(ModuleCore, OrderCore, deps.start)
	err := NewRuntime(r, Options{}).Start(context.Background(),
		Settings{WorkerCore: config.WorkerCore{StateDir: t.TempDir(), TmuxSocket: "/tmp/x/s", Name: "w"}})
	if !errors.Is(err, hostErr) || !strings.Contains(err.Error(), "terminal host") {
		t.Fatalf("err = %v", err)
	}
	if _, lerr := opened.LoadShare(); lerr == nil {
		t.Fatal("the store stayed open after a failed start")
	}

	// An unusable state directory is named.
	deps.openStore = func(context.Context, string) (*store.Store, error) { return nil, errors.New("read-only") }
	r = NewRegistry()
	_ = r.Register(ModuleCore, OrderCore, deps.start)
	err = NewRuntime(r, Options{}).Start(context.Background(), Settings{WorkerCore: config.WorkerCore{StateDir: "/nope"}})
	if err == nil || !strings.Contains(err.Error(), "/nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultRegistryHasTheCoreModuleFirst(t *testing.T) {
	// Only the lifecycle lock (Story 4.10) starts before the core module: the
	// state is never opened without it.
	names := DefaultRegistry.Names()
	if len(names) < 2 || names[0] != ModuleLifecycle || names[1] != ModuleCore {
		t.Fatalf("default modules = %v, want lifecycle, then core", names)
	}
}

func TestShareModeOf(t *testing.T) {
	for in, want := range map[string]store.ShareMode{"all": store.ShareAll, "none": store.ShareNone, "later": store.ShareLater} {
		if got, err := shareModeOf(in); err != nil || got != want {
			t.Errorf("%s = %v, %v", in, got, err)
		}
	}
	if _, err := shareModeOf("select"); err == nil {
		t.Error("select must be refused: names come from the wizard or the Hub")
	}
}
