package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func pairedStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	if _, err := enr.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	hub, _ := identity.GenerateKey()
	if err := enr.Commit(context.Background(), agent.Enrollment{Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w1", Name: "pi", Owner: "alice", Kind: "private"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := `{"server":"https://hub.example","name":"pi"}`
	if err := os.WriteFile(filepath.Join(dir, workerConfigName), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runReset(t *testing.T, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	deps := defaultWorkerResetDeps()
	deps.stdin = strings.NewReader(stdin)
	deps.lookupEnv = func(string) (string, bool) { return "", false }
	deps.workerRunning = func() (string, bool) { return "", false }
	cmd := newWorkerResetCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"--state-dir", dir}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func keyExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, agent.WorkerKeyFile))
	return err == nil
}

func TestWorkerResetWithYesWipesPairingAndKeyAndConfigServer(t *testing.T) {
	dir := pairedStateDir(t)
	out, err := runReset(t, dir, "", "--yes")
	if err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}
	if keyExists(dir) {
		t.Error("the key pair survived the reset")
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, _ := agent.NewEnrollments(st, dir).Load(context.Background()); ok {
		t.Error("the enrollment survived the reset")
	}
	cfg, err := config.LoadFile(filepath.Join(dir, workerConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cfg.Lookup(config.KeyServer); ok {
		t.Error("the Hub address survived the reset")
	}
	if _, ok, _ := cfg.Lookup(config.KeyName); !ok {
		t.Error("a machine setting (name) was removed")
	}
	if !strings.Contains(out, "Worker reset") || !strings.Contains(out, "remove it there") {
		t.Errorf("output = %q", out)
	}
}

func TestWorkerResetAsksAndDeclinesByDefault(t *testing.T) {
	for _, in := range []string{"", "n\n", "no\n", "\n"} {
		dir := pairedStateDir(t)
		out, err := runReset(t, dir, in)
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("stdin %q: err = %v", in, err)
		}
		if !keyExists(dir) {
			t.Errorf("stdin %q: the key was deleted without confirmation", in)
		}
		if !strings.Contains(out, "Continue?") {
			t.Errorf("no question asked: %q", out)
		}
	}
}

func TestWorkerResetProceedsAfterConfirmation(t *testing.T) {
	dir := pairedStateDir(t)
	if _, err := runReset(t, dir, "y\n"); err != nil {
		t.Fatal(err)
	}
	if keyExists(dir) {
		t.Error("the key survived a confirmed reset")
	}
}

func TestWorkerResetOfAnUnconfiguredWorkerIsANoOp(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none")
	out, err := runReset(t, missing, "", "--yes")
	if err != nil || !strings.Contains(out, "Nothing to reset") {
		t.Fatalf("err=%v out=%q", err, out)
	}
	dir := pairedStateDir(t)
	if _, err := runReset(t, dir, "", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, err = runReset(t, dir, "", "--yes")
	if err != nil || !strings.Contains(out, "Nothing to reset") {
		t.Fatalf("second reset: err=%v out=%q", err, out)
	}
}

func TestWorkerResetRefusesWhileAWorkerRunsAndChangesNothing(t *testing.T) {
	dir := pairedStateDir(t)
	deps := defaultWorkerResetDeps()
	deps.stdin = strings.NewReader("y\n")
	deps.lookupEnv = func(string) (string, bool) { return "", false }
	deps.workerRunning = func() (string, bool) { return "/run/user/1000/minion/worker.sock", true }
	cmd := newWorkerResetCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--state-dir", dir, "--yes"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stop it first") || !strings.Contains(err.Error(), "worker.sock") {
		t.Fatalf("err = %v, want a refusal that names the socket", err)
	}
	if !keyExists(dir) {
		t.Error("a refused reset deleted the key")
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.LoadEnrollment(context.Background()); err != nil {
		t.Errorf("a refused reset deleted the enrollment: %v", err)
	}
}

// startFakeWorker starts the lifecycle module of the Worker runtime on dir and
// nothing else: no control socket, as on a Windows Worker. It returns the stop.
func startFakeWorker(t *testing.T, dir string) (func(), error) {
	t.Helper()
	r := agent.NewRegistry()
	if err := agent.RegisterLifecycle(r); err != nil {
		t.Fatal(err)
	}
	rt := agent.NewRuntime(r, agent.Options{})
	if err := rt.Start(context.Background(), agent.Settings{WorkerCore: config.WorkerCore{StateDir: dir}}); err != nil {
		return nil, err
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = rt.Stop(context.Background()) }) }
	t.Cleanup(stop)
	return stop, nil
}

func resetDepsWithoutSocket(stdin io.Reader) workerResetDeps {
	deps := defaultWorkerResetDeps()
	deps.stdin = stdin
	deps.lookupEnv = func(string) (string, bool) { return "", false }
	// A Worker with no control socket (Windows) is invisible to this probe.
	deps.workerRunning = func() (string, bool) { return "", false }
	return deps
}

func runResetDeps(dir string, deps workerResetDeps, args ...string) (string, error) {
	cmd := newWorkerResetCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"--state-dir", dir}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func assertPairingIntact(t *testing.T, dir string) {
	t.Helper()
	if !keyExists(dir) {
		t.Error("a refused reset deleted the key")
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.LoadEnrollment(context.Background()); err != nil {
		t.Errorf("a refused reset deleted the enrollment: %v", err)
	}
}

// A Worker that has no control socket (Windows) is found through the lifecycle
// lock; reset must refuse and delete nothing.
func TestWorkerResetRefusesAWorkerWithoutControlSocket(t *testing.T) {
	dir := pairedStateDir(t)
	stop, err := startFakeWorker(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--yes"}, {}} {
		_, err = runResetDeps(dir, resetDepsWithoutSocket(strings.NewReader("y\n")), args...)
		if err == nil || !strings.Contains(err.Error(), "stop it first") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("args %v: err = %v, want a refusal", args, err)
		}
		assertPairingIntact(t, dir)
	}
	stop()
	if _, err := runResetDeps(dir, resetDepsWithoutSocket(nil), "--yes"); err != nil {
		t.Fatalf("reset after the Worker stopped: %v", err)
	}
	if keyExists(dir) {
		t.Error("the key survived the reset")
	}
}

// onRead runs fn on the first Read: the moment the confirmation question
// waits for its answer.
type onRead struct {
	r    io.Reader
	once sync.Once
	fn   func()
}

func (o *onRead) Read(p []byte) (int, error) {
	o.once.Do(o.fn)
	return o.r.Read(p)
}

// The Worker starts after the initial probe, while the question waits: the
// answer yes must not delete the pairing under the new connection.
func TestWorkerResetRefusesAWorkerThatStartsWhileTheQuestionWaits(t *testing.T) {
	dir := pairedStateDir(t)
	var started bool
	in := &onRead{r: strings.NewReader("y\n"), fn: func() {
		_, err := startFakeWorker(t, dir) // the lock is free here: the early probe let go of it
		if err != nil {
			t.Errorf("a Worker could not start while the question waited: %v", err)
			return
		}
		started = true
	}}
	_, err := runResetDeps(dir, resetDepsWithoutSocket(in))
	if !started {
		t.Fatal("the Worker did not start")
	}
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	assertPairingIntact(t, dir)
}

// While the reset deletes, no Worker can start: it fails on the lock instead
// of connecting with a pairing that is about to disappear.
func TestWorkerStartIsRefusedWhileResetRuns(t *testing.T) {
	dir := pairedStateDir(t)
	deps := resetDepsWithoutSocket(nil)
	realOpen := deps.openStore
	var startErr error
	deps.openStore = func(ctx context.Context, d string) (agent.ResetStore, io.Closer, error) {
		_, startErr = startFakeWorker(t, d)
		return realOpen(ctx, d)
	}
	if _, err := runResetDeps(dir, deps, "--yes"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(startErr, agent.ErrStateInUse) {
		t.Fatalf("Worker start during reset: err = %v, want ErrStateInUse", startErr)
	}
	if keyExists(dir) {
		t.Error("the key survived the reset")
	}
	// After the reset the Worker starts again.
	if _, err := startFakeWorker(t, dir); err != nil {
		t.Fatalf("Worker start after reset: %v", err)
	}
}
