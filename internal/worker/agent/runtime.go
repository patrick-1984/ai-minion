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
	"syscall"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
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
	// Out is for human-readable reports, such as the result of enrollment.
	// Nil discards them.
	Out io.Writer
	// InviteArgvWarned is true when the caller (minion worker up) already
	// printed the process-list warning of a literal --invite-key; the
	// enrollment hook then stays silent. False (the default, "worker run")
	// leaves the warning to the hook.
	InviteArgvWarned bool
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

	mu       sync.Mutex
	state    runtimeState
	settings Settings
	services map[reflect.Type]service
	running  []started
}

// NewRuntime returns a runtime for the modules registered in reg.
func NewRuntime(reg *Registry, opts Options) *Runtime {
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = DefaultStopTimeout
	}
	if len(opts.Signals) == 0 {
		opts.Signals = []os.Signal{os.Interrupt, syscall.SIGTERM}
	}
	return &Runtime{reg: reg, opts: opts, services: map[reflect.Type]service{}}
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
	rt.mu.Unlock()

	fail := func(err error) error {
		// The cleanup must run although ctx may be cancelled already.
		return errors.Join(err, rt.stopAll(context.WithoutCancel(ctx)))
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
	return nil
}

// Stop stops the started modules in reverse order within the stop timeout and
// returns the failures joined. It is idempotent.
func (rt *Runtime) Stop(ctx context.Context) error {
	return rt.stopAll(ctx)
}

// stopAll stops the started modules in reverse order. One hard deadline,
// StopTimeout from now (or the earlier deadline of ctx), covers the whole
// sequence; the modules get a context that ends [stopGrace] before it.
func (rt *Runtime) stopAll(ctx context.Context) error {
	rt.mu.Lock()
	if rt.state == stateStopped {
		rt.mu.Unlock()
		return nil
	}
	rt.state = stateStopped
	running := rt.running
	rt.running = nil
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
	for i := len(running) - 1; i >= 0; i-- {
		r := running[i]
		if r.stop == nil {
			continue
		}
		if err := stopWithin(modCtx, hardCtx, r.stop); err != nil {
			errs = append(errs, fmt.Errorf("agent: module %q failed to stop: %w", r.module, err))
		}
		log.Debug("worker module stopped", "module", r.module)
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
	if err := rt.Start(sigCtx, s); err != nil {
		return err
	}
	<-sigCtx.Done()
	s.logger().Info("worker shutting down", "timeout", rt.opts.StopTimeout.String())
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
