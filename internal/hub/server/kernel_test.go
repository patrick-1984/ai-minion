package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type greeter interface{ Greet() string }

type english struct{}

func (english) Greet() string { return "hello" }

type counter struct{ n int }

func noop(context.Context, *Env) error { return nil }

func mustRegister(t *testing.T, r *Registry, name string, order int, init InitFunc, opts ...ModuleOption) {
	t.Helper()
	if err := r.Register(name, order, init, opts...); err != nil {
		t.Fatal(err)
	}
}

func TestModulesInitialiseInOrderThenName(t *testing.T) {
	r := NewRegistry()
	var got []string
	rec := func(name string) InitFunc {
		return func(context.Context, *Env) error { got = append(got, name); return nil }
	}
	// Registered in scrambled order; order 20 has a tie.
	mustRegister(t, r, "web", 30, rec("web"))
	mustRegister(t, r, "b-auth", 20, rec("b-auth"))
	mustRegister(t, r, "store", 10, rec("store"))
	mustRegister(t, r, "a-audit", 20, rec("a-audit"))

	if names := r.Names(); !reflect.DeepEqual(names, []string{"store", "a-audit", "b-auth", "web"}) {
		t.Fatalf("Names = %v", names)
	}
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"store", "a-audit", "b-auth", "web"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("init order = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(k.Modules(), want) {
		t.Fatalf("Modules = %v", k.Modules())
	}
}

func TestProvideAndRequireTypedService(t *testing.T) {
	r := NewRegistry()
	var got string
	mustRegister(t, r, "greeter", 10, func(_ context.Context, env *Env) error {
		return Provide[greeter](env, english{})
	}, DeclaresService[greeter]())
	mustRegister(t, r, "user", 20, func(_ context.Context, env *Env) error {
		g, err := Require[greeter](env)
		if err != nil {
			return err
		}
		got = g.Greet()
		return nil
	})
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Fatalf("got %q", got)
	}
	if g, ok := Lookup[greeter](k); !ok || g.Greet() != "hello" {
		t.Fatalf("Lookup = %v, %v", g, ok)
	}
	if _, ok := Lookup[*counter](k); ok {
		t.Fatal("Lookup found a service nobody provided")
	}
}

func TestServicesAreKeyedByExactType(t *testing.T) {
	r := NewRegistry()
	c := &counter{n: 3}
	mustRegister(t, r, "p", 10, func(_ context.Context, env *Env) error {
		if err := Provide(env, c); err != nil { // *counter
			return err
		}
		return Provide(env, 42) // int
	}, DeclaresService[*counter](), DeclaresService[int]())
	mustRegister(t, r, "q", 20, func(_ context.Context, env *Env) error {
		got, err := Require[*counter](env)
		if err != nil || got != c {
			t.Errorf("Require[*counter] = %v, %v", got, err)
		}
		n, err := Require[int](env)
		if err != nil || n != 42 {
			t.Errorf("Require[int] = %v, %v", n, err)
		}
		if _, err := Require[counter](env); err == nil {
			t.Error("Require[counter] must not match a *counter service")
		}
		return nil
	})
	if err := NewKernel(r).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMissingServiceNamesBothModulesWhenDeclared(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "greeter", 20, func(_ context.Context, env *Env) error {
		return Provide[greeter](env, english{})
	}, DeclaresService[greeter]())
	mustRegister(t, r, "early-user", 10, func(_ context.Context, env *Env) error {
		_, err := Require[greeter](env)
		return err
	})
	err := NewKernel(r).Start(context.Background())
	if err == nil {
		t.Fatal("want startup failure")
	}
	msg := err.Error()
	for _, want := range []string{`"early-user"`, `"greeter"`, "server.greeter", "initialises later"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
}

func TestMissingServiceDeclaredButNotProvided(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "greeter", 10, noop, DeclaresService[greeter]())
	mustRegister(t, r, "user", 20, func(_ context.Context, env *Env) error {
		_, err := Require[greeter](env)
		return err
	})
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"user"`) || !strings.Contains(err.Error(), `"greeter"`) ||
		!strings.Contains(err.Error(), "did not provide it") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingServiceNobodyDeclares(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "first", 10, noop)
	mustRegister(t, r, "second", 20, func(_ context.Context, env *Env) error {
		_, err := Require[greeter](env)
		return err
	})
	err := NewKernel(r).Start(context.Background())
	if err == nil {
		t.Fatal("want startup failure")
	}
	for _, want := range []string{`module "second" requires service server.greeter`, "no registered module declares service server.greeter", `module "second" failed to initialise`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "no module provides it") {
		t.Errorf("error %q still carries the misleading wording", err)
	}
}

func TestProvideOfUndeclaredTypeFailsNamingModuleAndType(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "forgetful", 10, func(_ context.Context, env *Env) error {
		return Provide[greeter](env, english{})
	})
	err := NewKernel(r).Start(context.Background())
	if err == nil {
		t.Fatal("want startup failure")
	}
	for _, want := range []string{`"forgetful"`, "server.greeter", "did not declare", "DeclaresService"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestDuplicateDeclarationIsRefusedAtRegister(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "one", 10, noop, DeclaresService[greeter]())
	err := r.Register("two", 20, noop, DeclaresService[greeter]())
	if err == nil {
		t.Fatal("second declaration of the same service accepted")
	}
	for _, want := range []string{`"one"`, `"two"`, "server.greeter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if names := r.Names(); !reflect.DeepEqual(names, []string{"one"}) {
		t.Fatalf("the rejected module was registered: %v", names)
	}
	// Declaring the same type twice within one module is harmless.
	if err := r.Register("three", 30, noop, DeclaresService[*counter](), DeclaresService[*counter]()); err != nil {
		t.Fatal(err)
	}
}

func TestProviderOrderedTooLateIsNamedWithRequirer(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "late-provider", 50, func(_ context.Context, env *Env) error {
		return Provide[greeter](env, english{})
	}, DeclaresService[greeter]())
	mustRegister(t, r, "requirer", 10, func(_ context.Context, env *Env) error {
		_, err := Require[greeter](env)
		return err
	})
	err := NewKernel(r).Start(context.Background())
	if err == nil {
		t.Fatal("want startup failure")
	}
	for _, want := range []string{`"requirer"`, `"late-provider"`, "server.greeter", "initialises later"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestProvideNilInterfaceIsRefused(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "nil-provider", 10, func(_ context.Context, env *Env) error {
		return Provide[greeter](env, nil)
	}, DeclaresService[greeter]())
	err := NewKernel(r).Start(context.Background())
	if err == nil {
		t.Fatal("want startup failure")
	}
	for _, want := range []string{`"nil-provider"`, "server.greeter", "nil"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestDuplicateProviderIsRefused(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "one", 10, func(_ context.Context, env *Env) error {
		if err := Provide[greeter](env, english{}); err != nil {
			return err
		}
		return Provide[greeter](env, english{})
	}, DeclaresService[greeter]())
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"one"`) || !strings.Contains(err.Error(), "already provides") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("", 1, noop); err == nil {
		t.Error("empty name accepted")
	}
	if err := r.Register("x", 1, nil); err == nil {
		t.Error("nil init accepted")
	}
	mustRegister(t, r, "x", 1, noop)
	if err := r.Register("x", 2, noop); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate accepted: %v", err)
	}
}

func TestRegisterModuleUsesDefaultRegistryAndPanicsOnMisuse(t *testing.T) {
	saved := DefaultRegistry
	DefaultRegistry = NewRegistry()
	defer func() { DefaultRegistry = saved }()

	RegisterModule("mod-a", 5, noop)
	if got := DefaultRegistry.Names(); !reflect.DeepEqual(got, []string{"mod-a"}) {
		t.Fatalf("Names = %v", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate RegisterModule must panic")
		}
	}()
	RegisterModule("mod-a", 6, noop)
}

func TestStartHooksRunInOrderAndStopHooksInReverse(t *testing.T) {
	r := NewRegistry()
	var log []string
	mod := func(name string, order int) {
		mustRegister(t, r, name, order, func(_ context.Context, env *Env) error {
			log = append(log, "init:"+name)
			env.OnStart(func(context.Context) error { log = append(log, "start:"+name); return nil })
			env.OnStop(func(context.Context) error { log = append(log, "stop:"+name); return nil })
			return nil
		})
	}
	mod("c", 30)
	mod("a", 10)
	mod("b", 20)
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := k.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"init:a", "init:b", "init:c",
		"start:a", "start:b", "start:c",
		"stop:c", "stop:b", "stop:a",
	}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("log =\n%v\nwant\n%v", log, want)
	}
	// Stop is idempotent.
	if err := k.Stop(context.Background()); err != nil || len(log) != len(want) {
		t.Fatalf("second Stop ran hooks again or failed: %v %v", err, log)
	}
}

func TestInitFailureStopsEarlierModulesInReverse(t *testing.T) {
	r := NewRegistry()
	var log []string
	mod := func(name string, order int, fail bool) {
		mustRegister(t, r, name, order, func(_ context.Context, env *Env) error {
			if fail {
				return errors.New("boom")
			}
			env.OnStop(func(context.Context) error { log = append(log, "stop:"+name); return nil })
			return nil
		})
	}
	mod("a", 10, false)
	mod("b", 20, false)
	mod("c", 30, true)
	mod("d", 40, false)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `module "c" failed to initialise: boom`) {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(log, []string{"stop:b", "stop:a"}) {
		t.Fatalf("cleanup = %v", log)
	}
}

func TestStartHookFailureStopsEverything(t *testing.T) {
	r := NewRegistry()
	var log []string
	mustRegister(t, r, "a", 10, func(_ context.Context, env *Env) error {
		env.OnStart(func(context.Context) error { log = append(log, "start:a"); return nil })
		env.OnStop(func(context.Context) error { log = append(log, "stop:a"); return nil })
		return nil
	})
	mustRegister(t, r, "b", 20, func(_ context.Context, env *Env) error {
		env.OnStart(func(context.Context) error { return errors.New("port in use") })
		env.OnStop(func(context.Context) error { log = append(log, "stop:b"); return nil })
		return nil
	})
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `module "b" failed to start: port in use`) {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(log, []string{"start:a", "stop:b", "stop:a"}) {
		t.Fatalf("log = %v", log)
	}
}

func TestStopRunsAllHooksAndJoinsErrors(t *testing.T) {
	r := NewRegistry()
	ran := map[string]bool{}
	mod := func(name string, order int, stop HookFunc) {
		mustRegister(t, r, name, order, func(_ context.Context, env *Env) error {
			env.OnStop(func(ctx context.Context) error { ran[name] = true; return stop(ctx) })
			return nil
		})
	}
	mod("a", 10, func(context.Context) error { return nil })
	mod("b", 20, func(context.Context) error { return errors.New("b broke") })
	mod("c", 30, func(context.Context) error { panic("c panicked") })
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := k.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "b broke") || !strings.Contains(err.Error(), "c panicked") {
		t.Fatalf("err = %v", err)
	}
	if !ran["a"] || !ran["b"] || !ran["c"] {
		t.Fatalf("hooks that ran: %v", ran)
	}
}

func TestStartTwiceIsRefused(t *testing.T) {
	k := NewKernel(NewRegistry())
	if err := k.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := k.Start(context.Background()); err == nil {
		t.Fatal("second Start accepted")
	}
}

func TestStartWithCancelledContextRunsCleanup(t *testing.T) {
	r := NewRegistry()
	var stopped bool
	ctx, cancel := context.WithCancel(context.Background())
	mustRegister(t, r, "a", 10, func(_ context.Context, env *Env) error {
		env.OnStop(func(ctx context.Context) error {
			stopped = ctx.Err() == nil // cleanup must not see the cancelled context
			return nil
		})
		cancel()
		return nil
	})
	mustRegister(t, r, "b", 20, noop)
	err := NewKernel(r).Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !stopped {
		t.Fatal("stop hook did not run with a live context")
	}
}

func TestFailedStartCleanupIsBoundedAndRunsAllStopHooks(t *testing.T) {
	r := NewRegistry()
	var order []string
	mustRegister(t, r, "a", 10, func(_ context.Context, env *Env) error {
		env.OnStop(func(context.Context) error { order = append(order, "a"); return nil })
		return nil
	})
	mustRegister(t, r, "b", 20, func(_ context.Context, env *Env) error {
		// A graceful shutdown that waits for an active request.
		env.OnStop(func(ctx context.Context) error {
			order = append(order, "b")
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("cleanup context has no deadline")
			}
			<-ctx.Done()
			return ctx.Err()
		})
		return nil
	})
	mustRegister(t, r, "c", 30, func(context.Context, *Env) error { return errors.New("boom") })

	k := NewKernel(r)
	k.cleanupTimeout = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- k.Start(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "boom") || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the init failure joined with the expired cleanup", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return: failure cleanup has no deadline")
	}
	if !reflect.DeepEqual(order, []string{"b", "a"}) {
		t.Fatalf("stop order = %v, want b then a", order)
	}
}

func TestEnvModuleName(t *testing.T) {
	r := NewRegistry()
	var got string
	mustRegister(t, r, "named", 1, func(_ context.Context, env *Env) error { got = env.Module(); return nil })
	if err := NewKernel(r).Start(context.Background()); err != nil || got != "named" {
		t.Fatalf("got %q, %v", got, err)
	}
}
