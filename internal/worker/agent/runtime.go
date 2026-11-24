package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// DefaultStopTimeout is the hard bound of the whole shutdown after a signal
// (Story 2.7): every module stop, the abandoned ones included, is over when it
// has passed. The Worker also runs under Docker, whose stop sends SIGKILL after
// 10 s, so nothing in the stop sequence may wait past it. The modules get a
// context that ends [stopGrace] earlier.
const DefaultStopTimeout = 10 * time.Second

// Settings is what a module or a pre-start hook gets to know about the run:
// the core settings (the tmux socket is resolved once, to an absolute path,
// before any module starts), the resolver for settings the module declares
// itself (flag, environment, config file, default), the logger and a writer
// for messages meant for the person who started the Worker.
type Settings struct {
	config.WorkerCore

	// Resolver resolves further Worker settings with the same precedence
	// chain as the core settings. Nil in unit tests that do not need it.
	Resolver *config.Resolver
	// Logger receives operational messages; never terminal content, message
	// bodies or secrets (AD-20). Nil discards them.
	Logger *slog.Logger
	// LogLive switches the level of Logger at runtime and holds its recent
	// lines (worker_debug, worker_logs). Nil: no remote debug.
	LogLive *logx.Live
	// LogSinks is the state of the remote log sinks (syslog, otel) for
	// minion worker status; nil when there are none.
	LogSinks func() []ops.LogSinkStatus
	// StartLogSinks builds the remote log sinks; the log sinks module calls
	// it after the lifecycle lock, the update guard and the pre-start hooks
	// (enrollment). It never fails the start. Nil: none.
	StartLogSinks func(ctx context.Context)
	// TrafficTap observes the frames of the connection to the Hub (the
	// Worker traffic log); nil: none.
	TrafficTap transport.FrameTap
	// StopLogSinks flushes and stops the remote log sinks within ctx; the
	// log module calls it last in the stop sequence. Nil: nothing to stop.
	StopLogSinks func(ctx context.Context) error
	// StartBackground starts work that edits local files outside the state
	// directory (the Claude exporter installer). The exporter module calls it
	// after the lifecycle lock and the core start, so a duplicate Worker
	// never runs it, and calls the returned stop (which must end and join the
	// work) before the lock is released or a replacement Worker takes over.
	// Nil: nothing to start.
	StartBackground func(ctx context.Context) (stop func())
	// StartAfterReady starts work that may run only in a Worker that started
	// (the shell login block update): Start calls it once every module
	// started, never for a failed or interrupted start, and the stop
	// sequence calls the returned stop (which must end and join the work)
	// before any module stops, so before the lifecycle lock is released. It
	// must only start the work and return. Nil: nothing to start.
	StartAfterReady func(ctx context.Context) (stop func())
	// Out is for human-readable reports, such as the result of enrollment.
	// Nil discards them.
	Out io.Writer
	// InviteArgvWarned is true when the caller (minion worker up) already
	// printed the process-list warning of a literal --invite-key; the
	// enrollment hook then stays silent. False (the default, "worker run")
	// leaves the warning to the hook.
	InviteArgvWarned bool
	// InviteStdin supplies --invite-key - when a provisioning command hands off enrollment.
	InviteStdin io.Reader
}

func (s Settings) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// StopFunc stops what a module started. It must return when ctx ends; the
// runtime does not wait longer than the stop timeout for it.
type StopFunc func(ctx context.Context) error

// StartFunc starts one module. ctx ends when the Worker is shutting down. It
// may [Provide] services, [Require] the services of earlier modules and
// returns the function that stops the module (nil when there is nothing to
// stop). A module that fails to start has nothing to stop: it cleans up what
// it opened before it returns the error.
type StartFunc func(ctx context.Context, env *Env) (StopFunc, error)

// PreStartFunc is a hook that runs once before the modules start, for example
// invitation enrollment (Story 4.6). It reports to s.Out. A failing hook
// aborts the start.
type PreStartFunc func(ctx context.Context, s Settings) error

type module struct {
	name  string
	order int
	start StartFunc
}

type preStart struct {
	name string
	fn   PreStartFunc
}

// Registry holds the registered modules and pre-start hooks. Registration
// happens at process start, from the init functions of the mod_<name>.go
// files of this package, so a new module needs no edit of this file.
type Registry struct {
	mu      sync.Mutex
	modules map[string]*module
	pre     []preStart
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{modules: map[string]*module{}}
}

// DefaultRegistry is the process-wide registry that [RegisterModule] and
// [RegisterPreStart] use and that `minion worker run` hands to [NewRuntime].
var DefaultRegistry = NewRegistry()

// RegisterModule adds a module to the [DefaultRegistry]. It is meant to be
// called from the init function of internal/worker/agent/mod_<name>.go.
// Modules start in ascending order, ties broken by name, and stop in the
// reverse order. An empty or duplicate name or a nil start function is a
// programming error and panics.
func RegisterModule(name string, order int, start StartFunc) {
	if err := DefaultRegistry.Register(name, order, start); err != nil {
		panic(err)
	}
}

// RegisterPreStart adds a pre-start hook to the [DefaultRegistry]. Hooks run
// once per start, before any module, in registration order. `minion worker
// up` runs them too, so that enrollment completes and is reported before it
// exits. Misuse panics like [RegisterModule].
func RegisterPreStart(name string, fn PreStartFunc) {
	if err := DefaultRegistry.RegisterPreStart(name, fn); err != nil {
		panic(err)
	}
}

// Register adds a module to r. It fails for an empty or duplicate name and a
// nil start function.
func (r *Registry) Register(name string, order int, start StartFunc) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("agent: a module needs a name")
	}
	if start == nil {
		return fmt.Errorf("agent: module %q has no start function", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.modules[name]; dup {
		return fmt.Errorf("agent: module %q is registered twice", name)
	}
	r.modules[name] = &module{name: name, order: order, start: start}
	return nil
}

// RegisterPreStart adds a pre-start hook to r. It fails for an empty or
// duplicate name and a nil function.
func (r *Registry) RegisterPreStart(name string, fn PreStartFunc) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("agent: a pre-start hook needs a name")
	}
	if fn == nil {
		return fmt.Errorf("agent: pre-start hook %q has no function", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.pre {
		if p.name == name {
			return fmt.Errorf("agent: pre-start hook %q is registered twice", name)
		}
	}
	r.pre = append(r.pre, preStart{name: name, fn: fn})
	return nil
}

// has reports whether a module named name is registered.
func (r *Registry) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.modules[name]
	return ok
}

func (r *Registry) sorted() []*module {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*module, 0, len(r.modules))
	for _, m := range r.modules {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].order != out[j].order {
			return out[i].order < out[j].order
		}
		return out[i].name < out[j].name
	})
	return out
}

// Names returns the registered module names in start order.
func (r *Registry) Names() []string {
	var out []string
	for _, m := range r.sorted() {
		out = append(out, m.name)
	}
	return out
}

// PreStartNames returns the registered pre-start hook names in run order.
func (r *Registry) PreStartNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.pre))
	for _, p := range r.pre {
		out = append(out, p.name)
	}
	return out
}

// RunPreStart runs the pre-start hooks of r once, in registration order, and
// stops at the first failure, which names the hook. `minion worker up` calls
// it to complete enrollment before it installs a service; [Runtime.Start]
// calls it before the modules start.
func (r *Registry) RunPreStart(ctx context.Context, s Settings) error {
	r.mu.Lock()
	hooks := append([]preStart(nil), r.pre...)
	r.mu.Unlock()
	for _, h := range hooks {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("agent: start interrupted before pre-start hook %q: %w", h.name, err)
		}
		if err := safely(func() error { return h.fn(ctx, s) }); err != nil {
			return fmt.Errorf("agent: pre-start hook %q failed: %w", h.name, err)
		}
	}
	return nil
}

// Env is the view of the runtime that a module gets while it starts.
type Env struct {
	rt     *Runtime
	module string
}

// Module returns the name of the module that is starting.
func (e *Env) Module() string { return e.module }

// Settings returns the settings of this run.
func (e *Env) Settings() Settings {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.rt.settings
}

type service struct {
	value    any
	provider string
}

// Provide makes svc available to later modules as a service of type T. T is
// the type later modules ask for, normally an interface or a pointer to a
// struct. A second provider of the same T and a nil svc are errors.
func Provide[T any](env *Env, svc T) error {
	t := reflect.TypeFor[T]()
	if any(svc) == nil {
		return fmt.Errorf("agent: module %q cannot provide service %s: the service is nil", env.module, t)
	}
	rt := env.rt
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if prev, dup := rt.services[t]; dup {
		return fmt.Errorf("agent: module %q cannot provide service %s: module %q already provides it", env.module, t, prev.provider)
	}
	rt.services[t] = service{value: svc, provider: env.module}
	return nil
}

// Require returns the service of type T provided by an earlier module. The
// error names the requiring module, so a module that starts too early in the
// order is easy to find.
func Require[T any](env *Env) (T, error) {
	if v, ok := lookupService[T](env.rt); ok {
		return v, nil
	}
	var zero T
	return zero, fmt.Errorf("agent: module %q requires service %s, which no earlier module provided; give the providing module a lower order",
		env.module, reflect.TypeFor[T]())
}

// Lookup returns the service of type T from a started runtime, for the
// composition root and for tests.
func Lookup[T any](rt *Runtime) (T, bool) { return lookupService[T](rt) }

func lookupService[T any](rt *Runtime) (T, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s, ok := rt.services[reflect.TypeFor[T]()]
	if !ok {
		var zero T
		return zero, false
	}
	return s.value.(T), true
}

// Options tune a [Runtime]. The zero value is what the Worker uses.
type Options struct {
	// StopTimeout bounds the stop of all modules; zero means
	// [DefaultStopTimeout].
	StopTimeout time.Duration
	// Signals end [Runtime.Run]; nil means SIGINT and SIGTERM.
	Signals []os.Signal
}

type runtimeState int

const (
	stateNew runtimeState = iota
	stateStarted
	stateStopped
)

type started struct {
	module string
	stop   StopFunc
}

// Runtime starts the modules of a registry in order and stops them in the
// reverse order. Use one Runtime per Worker run.
type Runtime struct {
	reg  *Registry
	opts Options

	mu    sync.Mutex
	state runtimeState
	// startedAt is when Start began (the daemon start time).
	startedAt time.Time
	settings  Settings
	services  map[reflect.Type]service
	running   []started
	// held is the lifecycle lock taken at the very start of Start, before
	// any hook, so that the update crash guard runs only in the process that
	// owns the state; the lifecycle module takes it over.
	held io.Closer
	// stopReq is closed by RequestStop ("minion worker down", v0.9.3): Run
	// then stops like on a signal.
	stopReq  chan struct{}
	stopOnce sync.Once
	// guardStart is the start the update crash guard recorded for this run
	// of an uncommitted new binary (0: none); a clean stop takes it back.
	guardStart int64
	// ready is set once Start has started every module: only then may the
	// crash guard commit an update.
	ready atomic.Bool
	// instance is the random id of this daemon run (see [Runtime.Instance]).
	instance string
	// afterReady stops the work of Settings.StartAfterReady.
	afterReady func()
}

// Instance returns the id of this daemon run: one random id per Runtime that
// the local socket (daemon.instance) and the presence record share.
func (rt *Runtime) Instance() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.instance == "" {
		rt.instance = uuid.NewString()
	}
	return rt.instance
}

// Started reports whether the runtime started every module (Start returned
// without an error) and has not begun to stop.
func (e *Env) Started() bool { return e.rt != nil && e.rt.ready.Load() }

// RequestStop asks Run to stop the Worker, as a signal would.
func (rt *Runtime) RequestStop() { rt.stopOnce.Do(func() { close(rt.stopReq) }) }

// RequestStop asks the runtime to stop the Worker ("minion worker down").
func (e *Env) RequestStop() { e.rt.RequestStop() }

// NewRuntime returns a runtime for the modules registered in reg.
func NewRuntime(reg *Registry, opts Options) *Runtime {
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = DefaultStopTimeout
	}
	if len(opts.Signals) == 0 {
		opts.Signals = []os.Signal{os.Interrupt, syscall.SIGTERM}
	}
	return &Runtime{reg: reg, opts: opts, services: map[reflect.Type]service{}, stopReq: make(chan struct{})}
}

// Start runs the pre-start hooks, then starts every module in order. When a
// hook or a module fails, Start stops the modules that already run (in
// reverse order) and returns the error, which names the failing step. A
// runtime starts once.
func (rt *Runtime) Start(ctx context.Context, s Settings) error {
	rt.mu.Lock()
	if rt.state != stateNew {
		rt.mu.Unlock()
		return errors.New("agent: the runtime was already started")
	}
	rt.state = stateStarted
	rt.settings = s
	rt.startedAt = time.Now()
	rt.mu.Unlock()

	fail := func(err error) error {
		// The cleanup must run although ctx may be cancelled already. A
		// start that the operator interrupted (Ctrl+C, a service stop) and
		// whose cleanup succeeds is a clean stop, not a failed start.
		clean := ctx.Err() != nil && OnlyCanceled(err)
		return errors.Join(err, rt.stopAll(context.WithoutCancel(ctx), clean))
	}
	// The update crash guard runs first, before any hook can open or migrate
	// the database, and only under the state's lifecycle lock (a duplicate
	// Worker never touches the guard).
	if s.StateDir != "" && rt.reg.has(ModuleLifecycle) {
		waitForParent()
		lock, err := LockLifecycle(s.StateDir)
		if err != nil {
			return fail(fmt.Errorf("take the state lock in %s: %w", s.StateDir, err))
		}
		rt.mu.Lock()
		rt.held = lock
		rt.mu.Unlock()
		// A rollback hands over to the restored binary; the lifecycle lock
		// is released first, as the restored Worker takes it again.
		rolled, started, err := RecoverUpdate(ctx, s.logger(), s.StateDir, func() {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if rt.held != nil {
				_ = rt.held.Close()
				rt.held = nil
			}
		})
		if err != nil {
			return fail(err)
		}
		if rolled != nil {
			lastRollback.Store(rolled)
		}
		rt.mu.Lock()
		rt.guardStart = started
		rt.mu.Unlock()
	}
	if err := rt.reg.RunPreStart(ctx, s); err != nil {
		return fail(err)
	}
	for _, m := range rt.reg.sorted() {
		if err := ctx.Err(); err != nil {
			return fail(fmt.Errorf("agent: start interrupted before module %q: %w", m.name, err))
		}
		var stop StopFunc
		err := safely(func() error {
			var err error
			stop, err = m.start(ctx, &Env{rt: rt, module: m.name})
			return err
		})
		if err != nil {
			return fail(fmt.Errorf("agent: module %q failed to start: %w", m.name, err))
		}
		rt.mu.Lock()
		rt.running = append(rt.running, started{module: m.name, stop: stop})
		rt.mu.Unlock()
		s.logger().Debug("worker module started", "module", m.name)
	}
	rt.ready.Store(true)
	if s.StartAfterReady != nil {
		// Under the lock: a concurrent Stop either finds the work and
		// joins it, or comes first and the work never starts.
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if rt.state == stateStarted {
			rt.afterReady = s.StartAfterReady(ctx)
		}
	}
	return nil
}

// Stop stops the started modules in reverse order within the stop timeout and
// returns the failures joined. It is idempotent. It is a deliberate stop: when
// every module stopped without an error, the run does not count as a failed
// start of an uncommitted update.
func (rt *Runtime) Stop(ctx context.Context) error {
	return rt.stopAll(ctx, true)
}

// markCleanStop takes this run's start back from the update crash guard
// (cleanStop). It is called at most once, while this process still owns the
// state.
func (rt *Runtime) markCleanStop() {
	rt.mu.Lock()
	started, dir := rt.guardStart, rt.settings.StateDir
	rt.guardStart = 0
	rt.mu.Unlock()
	cleanStop(dir, version.Version, started)
}

// OnlyCanceled reports whether err is nothing but a clean cancellation (a
// signal that arrived while the Worker was starting). A joined error that also
// carries an independent failure, such as a module that failed to stop during
// the rollback, is not clean.
func OnlyCanceled(err error) bool {
	switch e := err.(type) {
	case nil:
		return true
	case interface{ Unwrap() []error }:
		for _, c := range e.Unwrap() {
			if !OnlyCanceled(c) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if next := e.Unwrap(); next != nil {
			return OnlyCanceled(next)
		}
		return false
	}
	return err == context.Canceled
}

// stopAll stops the started modules in reverse order. One hard deadline,
// StopTimeout from now (or the earlier deadline of ctx), covers the whole
// sequence; the modules get a context that ends [stopGrace] before it. With
// clean, a stop in which every module stopped without an error takes this
// run's start back from the update crash guard, right before the state's
// lifecycle lock is released (the lifecycle module, or the lock Start holds
// before that module runs).
func (rt *Runtime) stopAll(ctx context.Context, clean bool) error {
	rt.mu.Lock()
	if rt.state == stateStopped {
		rt.mu.Unlock()
		return nil
	}
	rt.state = stateStopped
	rt.ready.Store(false)
	held := rt.held
	rt.held = nil
	running := rt.running
	rt.running = nil
	afterReady := rt.afterReady
	rt.afterReady = nil
	log := rt.settings.logger()
	rt.mu.Unlock()

	hard := time.Now().Add(rt.opts.StopTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(hard) {
		hard = d
	}
	grace := stopGrace
	if total := time.Until(hard); grace > total/2 {
		grace = total / 2
	}
	hardCtx, cancelHard := context.WithDeadline(context.WithoutCancel(ctx), hard)
	defer cancelHard()
	modCtx, cancelMod := context.WithDeadline(ctx, hard.Add(-grace))
	defer cancelMod()

	var errs []error
	marked := false
	mark := func() {
		if clean && !marked && len(errs) == 0 {
			rt.markCleanStop()
		}
		marked = true
	}
	if afterReady != nil {
		if err := stopWithin(modCtx, hardCtx, func(context.Context) error { afterReady(); return nil }); err != nil {
			errs = append(errs, fmt.Errorf("agent: the work started after the start failed to stop: %w", err))
		}
	}
	for i := len(running) - 1; i >= 0; i-- {
		r := running[i]
		if r.module == ModuleLifecycle {
			mark()
		}
		if r.stop == nil {
			continue
		}
		if err := stopWithin(modCtx, hardCtx, r.stop); err != nil {
			errs = append(errs, fmt.Errorf("agent: module %q failed to stop: %w", r.module, err))
		}
		logx.BestEffortUntil(hardCtx, log, slog.LevelDebug, "worker module stopped", "module", r.module)
	}
	if held != nil {
		mark()
		_ = held.Close()
	}
	return errors.Join(errs...)
}

// Modules returns the names of the started modules, in start order.
func (rt *Runtime) Modules() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, len(rt.running))
	for i, r := range rt.running {
		out[i] = r.module
	}
	return out
}

// Run starts the Worker and runs it until ctx ends or one of the signals
// arrives (SIGINT and SIGTERM by default), then stops the modules within the
// stop timeout. A start failure is returned; a clean shutdown returns nil.
func (rt *Runtime) Run(ctx context.Context, s Settings) error {
	sigCtx, stop := signal.NotifyContext(ctx, rt.opts.Signals...)
	defer stop()
	stopFn := rt.RequestStop
	currentStop.Store(&stopFn)
	defer currentStop.CompareAndSwap(&stopFn, nil)
	if err := rt.Start(sigCtx, s); err != nil {
		return err
	}
	select {
	case <-sigCtx.Done():
	case <-rt.stopReq:
		logx.BestEffort(s.logger(), slog.LevelInfo, "stop requested over the local control socket (minion worker down)")
	}
	logx.BestEffort(s.logger(), slog.LevelInfo, "worker shutting down", "timeout", rt.opts.StopTimeout.String())
	// The Worker started and was asked to stop (Ctrl+C, worker down, a
	// service stop): once its modules stopped cleanly, this run is not a
	// failed start of an uncommitted update (Stop records that).
	return rt.Stop(context.WithoutCancel(ctx))
}

// stopWithin runs stop with ctx and gives up when hard ends, so that a module
// that ignores its context cannot hold the shutdown past the one deadline of
// the whole sequence: hard is shared by all modules and ctx ends [stopGrace]
// before it. The abandoned module keeps running in the background; the process
// is about to exit. A module reached after hard has passed still gets called
// and only [lateWait] to return (enough for a stop that needs no time, such as
// closing a file), so late modules add a small constant each, never a grace.
func stopWithin(ctx, hard context.Context, stop StopFunc) error {
	late := hard.Err() != nil
	done := make(chan error, 1)
	go func() { done <- safely(func() error { return stop(ctx) }) }()
	if !late {
		select {
		case err := <-done:
			return err
		case <-hard.Done():
			return fmt.Errorf("did not stop in time: %w", hard.Err())
		}
	}
	select {
	case err := <-done:
		return err
	case <-time.After(lateWait):
		return fmt.Errorf("did not stop in time: %w", hard.Err())
	}
}

const (
	// stopGrace is how long before the hard deadline the context of the
	// modules ends, so that a module that honours it can still return.
	stopGrace = 100 * time.Millisecond
	// lateWait is what a stop function gets once the hard deadline has passed.
	lateWait = time.Millisecond
)

// safely turns a panic of a module or hook into an error.
func safely(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn()
}

// StartedAt is when [Runtime.Start] began; zero before. It is the start time
// of the daemon in the local live status.
func (rt *Runtime) StartedAt() time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.startedAt
}
