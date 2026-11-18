package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
)

func lifecycleSettings(dir string) Settings {
	return Settings{WorkerCore: config.WorkerCore{StateDir: dir}}
}

func startLifecycleRuntime(t *testing.T, dir string) (*Runtime, error) {
	t.Helper()
	r := NewRegistry()
	if err := RegisterLifecycle(r); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{})
	return rt, rt.Start(context.Background(), lifecycleSettings(dir))
}

func TestLockLifecycleIsExclusiveAndReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	a, err := LockLifecycle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockLifecycle(dir); !errors.Is(err, ErrStateInUse) {
		t.Fatalf("second lock: err = %v, want ErrStateInUse", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	b, err := LockLifecycle(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = b.Close()
}

func TestLockLifecycleCreatesTheStateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "state")
	l, err := LockLifecycle(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if _, err := os.Stat(filepath.Join(dir, LifecycleLockFile)); err != nil {
		t.Errorf("no lock file: %v", err)
	}
}

func TestLifecycleModuleHoldsTheLockWhileTheWorkerRuns(t *testing.T) {
	dir := t.TempDir()
	rt, err := startLifecycleRuntime(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockLifecycle(dir); !errors.Is(err, ErrStateInUse) {
		t.Fatalf("a running Worker does not hold the lock: err = %v", err)
	}
	// A second Worker on the same state refuses to start.
	if _, err := startLifecycleRuntime(t, dir); !errors.Is(err, ErrStateInUse) {
		t.Fatalf("second Worker: err = %v, want ErrStateInUse", err)
	}
	if err := rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err := LockLifecycle(dir)
	if err != nil {
		t.Fatalf("the lock outlived the Worker: %v", err)
	}
	_ = l.Close()
}

func TestLifecycleModuleStartsBeforeTheCoreModule(t *testing.T) {
	if OrderLifecycle >= OrderCore {
		t.Fatalf("lifecycle order %d must be below core %d", OrderLifecycle, OrderCore)
	}
	names := DefaultRegistry.Names()
	li, ci := -1, -1
	for i, n := range names {
		switch n {
		case ModuleLifecycle:
			li = i
		case ModuleCore:
			ci = i
		}
	}
	if li < 0 || ci < 0 || li > ci {
		t.Fatalf("default registry order = %v", names)
	}
}

// A Worker run of an uncommitted update that starts and is then stopped
// cleanly takes its start back from the crash guard (boot counting).
func TestRuntimeCleanStopTakesBackTheGuardStart(t *testing.T) {
	dir := t.TempDir()
	if err := writePending(dir, pendingRecord{From: "v0.0.1", To: version.Version, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	if err := RegisterLifecycle(r); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx, lifecycleSettings(dir)) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, _ := readPending(dir); len(p.Starts) == 1 && len(rt.Modules()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run's start was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	p, ok := readPending(dir)
	if !ok || len(p.Starts) != 0 {
		t.Fatalf("after a clean stop: pending %v (ok %v), want the guard without starts", p, ok)
	}
}

// guardRuntime is a runtime with the lifecycle module and one more module, on
// a state with an uncommitted update of the running version.
func guardRuntime(t *testing.T, start StartFunc) (*Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	if err := writePending(dir, pendingRecord{From: "v0.0.1", To: version.Version, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	if err := RegisterLifecycle(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("probe", OrderLifecycle+1, start); err != nil {
		t.Fatal(err)
	}
	return NewRuntime(r, Options{StopTimeout: 2 * time.Second}), dir
}

func guardStarts(t *testing.T, dir string) int {
	t.Helper()
	p, ok := readPending(dir)
	if !ok {
		t.Fatal("the guard is gone")
	}
	return len(p.Starts)
}

// A stop in which a module fails is not clean: the start stays counted.
func TestRuntimeFailedStopKeepsTheGuardStart(t *testing.T) {
	rt, dir := guardRuntime(t, func(context.Context, *Env) (StopFunc, error) {
		return func(context.Context) error { return errors.New("stuck") }, nil
	})
	if err := rt.Start(context.Background(), lifecycleSettings(dir)); err != nil {
		t.Fatal(err)
	}
	if err := rt.Stop(context.Background()); err == nil {
		t.Fatal("the failing stop was not reported")
	}
	if n := guardStarts(t, dir); n != 1 {
		t.Fatalf("starts = %d after a failed stop, want 1", n)
	}
}

// Ctrl+C while the Worker is still starting, with a clean cleanup, is a clean
// stop: three of them never roll the update back.
func TestRuntimeInterruptedStartIsACleanStop(t *testing.T) {
	rt, dir := guardRuntime(t, func(ctx context.Context, _ *Env) (StopFunc, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Start(ctx, lifecycleSettings(dir)) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, _ := readPending(dir); len(p.Starts) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run's start was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !OnlyCanceled(err) {
		t.Fatalf("start error = %v, want only the cancellation", err)
	}
	if n := guardStarts(t, dir); n != 0 {
		t.Fatalf("starts = %d after an interrupted start, want 0", n)
	}
}

// A start that fails for a reason other than the operator's stop counts.
func TestRuntimeFailedStartKeepsTheGuardStart(t *testing.T) {
	rt, dir := guardRuntime(t, func(context.Context, *Env) (StopFunc, error) {
		return nil, errors.New("broken")
	})
	if err := rt.Start(context.Background(), lifecycleSettings(dir)); err == nil {
		t.Fatal("no start error")
	}
	if n := guardStarts(t, dir); n != 1 {
		t.Fatalf("starts = %d after a failed start, want 1", n)
	}
}

// Env.Started is false while modules are still starting and true afterwards,
// so the crash guard cannot commit an update before Start succeeded.
func TestEnvStartedOnlyAfterEveryModule(t *testing.T) {
	var env *Env
	var during bool
	rt, dir := guardRuntime(t, func(_ context.Context, e *Env) (StopFunc, error) {
		env, during = e, e.Started()
		return nil, nil
	})
	if err := rt.Start(context.Background(), lifecycleSettings(dir)); err != nil {
		t.Fatal(err)
	}
	if during || !env.Started() {
		t.Fatalf("Started during start = %v, after = %v", during, env.Started())
	}
	if err := rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.Started() {
		t.Fatal("Started after Stop")
	}
}
