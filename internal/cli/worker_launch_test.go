package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// fakeRuns is a Worker that ends with the given exit codes in turn, each
// after running for the given time.
type fakeRuns struct {
	codes  []int
	errs   []error
	ran    []time.Duration
	clock  time.Time
	slept  []time.Duration
	starts int
}

func (f *fakeRuns) supervisor(log *strings.Builder) supervisor {
	return supervisor{
		run: func(context.Context) (int, error) {
			i := f.starts
			f.starts++
			if i < len(f.ran) {
				f.clock = f.clock.Add(f.ran[i])
			}
			var err error
			if i < len(f.errs) {
				err = f.errs[i]
			}
			return f.codes[i], err
		},
		sleep: func(_ context.Context, d time.Duration) { f.slept = append(f.slept, d); f.clock = f.clock.Add(d) },
		now:   func() time.Time { return f.clock },
		log:   log,
	}
}

func TestSuperviseRestartsUntilACleanStop(t *testing.T) {
	f := &fakeRuns{codes: []int{1, 1, agent.ExitRestart, 2, 1, 1, 1, 1, 0}}
	var log strings.Builder
	f.supervisor(&log).supervise(context.Background())
	if f.starts != 9 {
		t.Errorf("started %d times", f.starts)
	}
	// 5 s, doubling, up to one minute; a restart request goes at once and
	// starts the delays over.
	want := []time.Duration{5 * time.Second, 10 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute}
	if !reflect.DeepEqual(f.slept, want) {
		t.Errorf("delays = %v, want %v", f.slept, want)
	}
	for _, s := range []string{"exit status 1", "asked for a restart", "the Worker stopped"} {
		if !strings.Contains(log.String(), s) {
			t.Errorf("log lacks %q:\n%s", s, log.String())
		}
	}
}

func TestSuperviseResetsTheDelayAfterAHealthyRun(t *testing.T) {
	f := &fakeRuns{codes: []int{1, 1, 1, 0}, ran: []time.Duration{0, 0, 2 * time.Minute, 0}}
	f.supervisor(&strings.Builder{}).supervise(context.Background())
	want := []time.Duration{5 * time.Second, 10 * time.Second, 5 * time.Second}
	if !reflect.DeepEqual(f.slept, want) {
		t.Errorf("delays = %v, want %v", f.slept, want)
	}
}

func TestSuperviseRetriesAWorkerThatCannotStartAndEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRuns{codes: []int{-1, -1, -1}, errs: []error{errors.New("no exe"), errors.New("no exe"), errors.New("no exe")}}
	s := f.supervisor(&strings.Builder{})
	s.sleep = func(context.Context, time.Duration) {
		if f.starts == 2 {
			cancel()
		}
	}
	s.supervise(ctx)
	if f.starts != 2 {
		t.Errorf("started %d times", f.starts)
	}
}

func TestLaunchArgsAreWhatFollowsLaunch(t *testing.T) {
	got := launchArgs([]string{`C:\m\minion.exe`, "worker", "launch", "--state-dir", `D:\s`})
	if !reflect.DeepEqual(got, []string{"--state-dir", `D:\s`}) {
		t.Errorf("args = %q", got)
	}
	if launchArgs([]string{"minion", "worker", "run"}) != nil {
		t.Error("args without launch")
	}
}

// The log of the supervisor and its Worker is appended to; a large one is
// moved to .1 at start.
func TestOpenLaunchLogRotatesALargeLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, launchLogName)
	if err := os.WriteFile(p, make([]byte, maxLaunchLog+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLaunchLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("new\n")
	_ = f.Close()
	if b, _ := os.ReadFile(p); string(b) != "new\n" {
		t.Errorf("log = %q", b)
	}
	if fi, err := os.Stat(p + ".1"); err != nil || fi.Size() != maxLaunchLog+1 {
		t.Errorf("rotated: %v", err)
	}
	f, _ = openLaunchLog(dir)
	_, _ = f.WriteString("more\n")
	_ = f.Close()
	if b, _ := os.ReadFile(p); string(b) != "new\nmore\n" {
		t.Errorf("log = %q", b)
	}
}

func TestSleepCtxEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sleepCtx(ctx, time.Hour) // returns at once
}

// The supervised Worker gets none of the MINION_ variables of the shell that
// started "worker up", so a variable cannot override what the user saved.
func TestLaunchEnvDropsEveryMinionVariable(t *testing.T) {
	env := launchEnv([]string{"PATH=/bin", "MINION_SHARE_EXISTING=all", "minion_invite_key=secret", "MINION_SUPERVISED=0", "HOME=/h"})
	want := []string{"PATH=/bin", "HOME=/h", "MINION_SUPERVISED=1"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
}

func TestHasStateDirArg(t *testing.T) {
	for args, want := range map[string]bool{"": false, "--debug": false, "--state-dir x": true, "--state-dir=x": true} {
		if got := hasStateDirArg(strings.Fields(args)); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
}

// A stop request during the back-off cancels the pending restart: the
// supervisor ends instead of bringing the Worker back.
func TestSuperviseStopRequestCancelsAPendingRestart(t *testing.T) {
	stop := make(chan struct{})
	var starts atomic.Int32
	s := supervisor{
		stop: stop,
		run: func(context.Context) (int, error) {
			starts.Add(1)
			return 1, nil
		},
		sleep: func(ctx context.Context, _ time.Duration) {
			close(stop)
			<-ctx.Done() // the back-off ends with the request
		},
		now: time.Now,
		log: &strings.Builder{},
	}
	done := make(chan struct{})
	go func() { s.supervise(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not end")
	}
	if starts.Load() != 1 {
		t.Errorf("the Worker was started %d times", starts.Load())
	}
}

// A stop request while the Worker starts (no control pipe yet) is retried
// until the Worker answers and ends; the supervisor then ends too.
func TestSuperviseStopRequestRetriesUntilTheWorkerEnds(t *testing.T) {
	stop := make(chan struct{})
	var asks atomic.Int32
	ended := make(chan struct{})
	s := supervisor{
		stop:      stop,
		stopEvery: time.Millisecond,
		askStop: func() {
			if asks.Add(1) == 3 {
				close(ended)
			}
		},
		run: func(context.Context) (int, error) {
			close(stop)
			<-ended
			return 1, nil // ended with an error, but a stop was requested
		},
		sleep: func(context.Context, time.Duration) { t.Error("slept") },
		now:   time.Now,
	}
	done := make(chan struct{})
	go func() { s.supervise(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not end")
	}
}

// A Worker that ignores the request is forced after the grace period.
func TestSuperviseStopRequestForcesAWorkerThatIgnoresIt(t *testing.T) {
	stop := make(chan struct{})
	s := supervisor{
		stop:      stop,
		stopGrace: 20 * time.Millisecond,
		stopEvery: time.Millisecond,
		run: func(ctx context.Context) (int, error) {
			close(stop)
			<-ctx.Done()
			return -1, ctx.Err()
		},
		sleep: func(context.Context, time.Duration) {},
		now:   time.Now,
	}
	done := make(chan struct{})
	go func() { s.supervise(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not end")
	}
}

// The log holds the Worker's console output (a pairing code): an existing,
// permissive log is replaced by a private one.
func TestOpenLaunchLogIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the DACL is checked by the Windows-tagged tests of internal/identity")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, launchLogName)
	if err := os.WriteFile(p, []byte("old pairing code MN-AAAA-BBBB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openLaunchLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("new\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, err %v", fi.Mode(), err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "MN-AAAA") {
		t.Errorf("the permissive log was kept: %q", b)
	}
}

func TestLaunchAndRunRefuseToRunAsSystem(t *testing.T) {
	old := systemAccount
	systemAccount = func() bool { return true }
	defer func() { systemAccount = old }()
	for _, cmd := range []*cobra.Command{newWorkerLaunchCmd(), newWorkerRunCmd(workerRunDeps{stderr: &strings.Builder{}})} {
		cmd.SetContext(context.Background())
		cmd.SetArgs(nil)
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		if err := cmd.Execute(); !errors.Is(err, service.ErrRunsAsSystem) {
			t.Errorf("%s: err = %v", cmd.Name(), err)
		}
	}
}
