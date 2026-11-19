package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
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
