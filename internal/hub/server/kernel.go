package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// InitFunc initialises one module. It may [Provide] services, [Require]
// services of earlier modules and register start and stop hooks on env.
type InitFunc func(ctx context.Context, env *Env) error

// HookFunc is a start or stop hook of a module.
type HookFunc func(ctx context.Context) error

// ModuleOption declares extra facts about a module at registration.
type ModuleOption func(*module)

// DeclaresService states that the module provides a service of type T. The
// declaration is mandatory: [Provide] refuses a type its module did not
// declare, and [Registry.Register] refuses a second module that declares the
// same T. Because every provider is known before any module initialises, the
// error of a missing service can always name the module that should provide T
// and say when that module comes too late in the init order.
func DeclaresService[T any]() ModuleOption {
	return func(m *module) {
		t := typeOf[T]()
		if !m.declaresType(t) {
			m.declares = append(m.declares, t)
		}
	}
}

type module struct {
	name     string
	order    int
	init     InitFunc
	declares []reflect.Type
}

func (m *module) declaresType(t reflect.Type) bool {
	for _, d := range m.declares {
		if d == t {
			return true
		}
	}
	return false
}

// Registry holds the registered modules. Registration happens at process
// start (from init functions); a [Kernel] reads the registry once.
type Registry struct {
	mu      sync.Mutex
	modules map[string]*module
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{modules: map[string]*module{}}
}

// DefaultRegistry is the process-wide registry that [RegisterModule] uses
// and that the Hub's composition root hands to [NewKernel].
var DefaultRegistry = NewRegistry()

// RegisterModule adds a module to the [DefaultRegistry]. It is meant to be
// called from the init function of internal/hub/server/mod_<name>.go. Modules
// initialise in ascending order, ties broken by name. An empty or duplicate
// name, or a nil init function, is a programming error and panics.
func RegisterModule(name string, order int, init InitFunc, opts ...ModuleOption) {
	if err := DefaultRegistry.Register(name, order, init, opts...); err != nil {
		panic(err)
	}
}

// Register adds a module to r. It fails for an empty or duplicate name, a nil
// init function, or a service type that another module already declares.
func (r *Registry) Register(name string, order int, init InitFunc, opts ...ModuleOption) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("server: a module needs a name")
	}
	if init == nil {
		return fmt.Errorf("server: module %q has no init function", name)
	}
	m := &module{name: name, order: order, init: init}
	for _, o := range opts {
		o(m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.modules[name]; dup {
		return fmt.Errorf("server: module %q is registered twice", name)
	}
	for _, d := range m.declares {
		for _, other := range r.modules {
			if other.declaresType(d) {
				return fmt.Errorf("server: module %q declares service %s, but module %q already declares it; a service has one provider",
					name, d, other.name)
			}
		}
	}
	r.modules[name] = m
	return nil
}

// sorted returns the modules in init order.
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

// Names returns the registered module names in init order.
func (r *Registry) Names() []string {
	var out []string
	for _, m := range r.sorted() {
		out = append(out, m.name)
	}
	return out
}

type service struct {
	value    any
	provider string
}

type hook struct {
	module string
	fn     HookFunc
}

// Kernel initialises the modules of a registry, holds their services and
// runs their start and stop hooks. Use one Kernel per Hub run; it is not
// safe for concurrent use.
type Kernel struct {
	mods     []*module
	services map[reflect.Type]service
	starts   []hook
	stops    []hook
	inited   []string
	state    kernelState

	// cleanupTimeout bounds the stop hooks that run after a failed Start.
	cleanupTimeout time.Duration
}

// defaultCleanupTimeout is how long the cleanup after a failed Start may take.
const defaultCleanupTimeout = 30 * time.Second

type kernelState int

const (
	stateNew kernelState = iota
	stateStarted
	stateStopped
)

// NewKernel returns a kernel for the modules registered in r.
func NewKernel(r *Registry) *Kernel {
	return &Kernel{mods: r.sorted(), services: map[reflect.Type]service{}, cleanupTimeout: defaultCleanupTimeout}
}

// Env is the view of the kernel that a module gets while it initialises.
type Env struct {
	k      *Kernel
	module *module
}

// Module returns the name of the module that is initialising.
func (e *Env) Module() string { return e.module.name }

// OnStart registers a hook that runs after every module is initialised, in
// module order. A failing start hook aborts startup.
func (e *Env) OnStart(fn HookFunc) {
	e.k.starts = append(e.k.starts, hook{module: e.module.name, fn: fn})
}

// OnStop registers a cleanup hook. Stop runs the hooks in reverse
// registration order, so a module stops before the modules it depends on.
// The hooks also run when startup fails half way, for the modules that had
// registered them by then.
func (e *Env) OnStop(fn HookFunc) {
	e.k.stops = append(e.k.stops, hook{module: e.module.name, fn: fn})
}

func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

// Provide makes svc available to later modules as a service of type T. T is
// the type later modules ask for, normally an interface; Provide[Store] and
// Provide[*Store] are different services. A second provider of the same T is
// an error, and so is a nil svc (a nil interface value would make every
// [Require] fail later) and a type that the module did not declare with
// [DeclaresService].
func Provide[T any](env *Env, svc T) error {
	t := typeOf[T]()
	if !env.module.declaresType(t) {
		return fmt.Errorf("server: module %q cannot provide service %s: it did not declare it; add DeclaresService[%s]() to its RegisterModule call",
			env.module.name, t, t)
	}
	if any(svc) == nil {
		return fmt.Errorf("server: module %q cannot provide service %s: the service is nil", env.module.name, t)
	}
	if prev, dup := env.k.services[t]; dup {
		return fmt.Errorf("server: module %q cannot provide service %s: module %q already provides it",
			env.module.name, t, prev.provider)
	}
	env.k.services[t] = service{value: svc, provider: env.module.name}
	return nil
}

// Require returns the service of type T provided by an earlier module. When
// there is none, the error names the requiring module and the module that
// declared T, whether it comes too late in the init order or did not provide
// T when it ran.
func Require[T any](env *Env) (T, error) {
	t := typeOf[T]()
	if s, ok := env.k.services[t]; ok {
		return s.value.(T), nil
	}
	var zero T
	return zero, env.k.missing(env.module, t)
}

// Lookup returns the service of type T, for the composition root after
// startup.
func Lookup[T any](k *Kernel) (T, bool) {
	s, ok := k.services[typeOf[T]()]
	if !ok {
		var zero T
		return zero, false
	}
	return s.value.(T), true
}

func (k *Kernel) missing(requirer *module, t reflect.Type) error {
	for _, m := range k.mods {
		for _, d := range m.declares {
			if d != t {
				continue
			}
			if m.name == requirer.name {
				return fmt.Errorf("server: module %q requires service %s, but declares it provides it itself and has not provided it yet", requirer.name, t)
			}
			if k.hasInited(m.name) {
				return fmt.Errorf("server: module %q requires service %s from module %q, which initialised (order %d) but did not provide it",
					requirer.name, t, m.name, m.order)
			}
			return fmt.Errorf("server: module %q (order %d) requires service %s from module %q (order %d), which initialises later; give %q a lower order than %q",
				requirer.name, requirer.order, t, m.name, m.order, m.name, requirer.name)
		}
	}
	return fmt.Errorf("server: module %q requires service %s, but no registered module declares service %s",
		requirer.name, t, t)
}

func (k *Kernel) hasInited(name string) bool {
	for _, n := range k.inited {
		if n == name {
			return true
		}
	}
	return false
}

// Start initialises every module in order, then runs the start hooks in
// module order. If a module's init or start hook fails, Start stops what was
// set up (running the registered stop hooks in reverse) and returns the
// error, which names the failing module. A kernel starts once.
func (k *Kernel) Start(ctx context.Context) error {
	if k.state != stateNew {
		return errors.New("server: the kernel was already started")
	}
	k.state = stateStarted
	fail := func(err error) error {
		// Cleanup must run even when ctx is already cancelled, but it must
		// not hang on a hook that waits for work to finish (a graceful HTTP
		// shutdown), so it gets its own deadline.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), k.cleanupTimeout)
		defer cancel()
		if stopErr := k.Stop(cctx); stopErr != nil {
			return errors.Join(err, stopErr)
		}
		return err
	}
	for _, m := range k.mods {
		if err := ctx.Err(); err != nil {
			return fail(fmt.Errorf("server: startup interrupted before module %q: %w", m.name, err))
		}
		if err := m.init(ctx, &Env{k: k, module: m}); err != nil {
			return fail(fmt.Errorf("server: module %q failed to initialise: %w", m.name, err))
		}
		k.inited = append(k.inited, m.name)
	}
	for _, h := range k.starts {
		if err := ctx.Err(); err != nil {
			return fail(fmt.Errorf("server: startup interrupted before module %q: %w", h.module, err))
		}
		if err := h.fn(ctx); err != nil {
			return fail(fmt.Errorf("server: module %q failed to start: %w", h.module, err))
		}
	}
	return nil
}

// Stop runs the stop hooks in reverse registration order. It runs every
// hook, also after a failure, and returns the failures joined. Stop is
// idempotent: the second call does nothing.
func (k *Kernel) Stop(ctx context.Context) error {
	if k.state == stateStopped {
		return nil
	}
	k.state = stateStopped
	var errs []error
	for i := len(k.stops) - 1; i >= 0; i-- {
		h := k.stops[i]
		if err := runHook(ctx, h.fn); err != nil {
			errs = append(errs, fmt.Errorf("server: module %q failed to stop: %w", h.module, err))
		}
	}
	return errors.Join(errs...)
}

// Modules returns the names of the modules initialised so far, in order.
func (k *Kernel) Modules() []string { return append([]string(nil), k.inited...) }

// runHook calls fn and turns a panic into an error so that one broken stop
// hook cannot keep the others from running.
func runHook(ctx context.Context, fn HookFunc) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(ctx)
}

// Clone returns a registry with the same modules, so that a composition test
// (outside this package) can start the Hub a default process runs and add a
// module of its own without changing [DefaultRegistry].
func (r *Registry) Clone() *Registry { return r.clone() }

// clone returns a registry with the same modules, so that a test can start the
// composition a default Hub has and add a module of its own.
func (r *Registry) clone() *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := NewRegistry()
	for n, m := range r.modules {
		cp := *m
		c.modules[n] = &cp
	}
	return c
}
