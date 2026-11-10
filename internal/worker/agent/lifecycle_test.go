package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
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
