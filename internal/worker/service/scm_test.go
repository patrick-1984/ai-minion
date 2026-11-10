package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	ksvc "github.com/kardianos/service"
)

// fakeSCM plays the Windows service control manager: it calls Start, waits
// for the test to ask for a stop, then calls Stop, like kardianos' handler.
type fakeSCM struct {
	prog     ksvc.Interface
	cfg      *ksvc.Config
	stop     chan struct{}
	started  chan struct{}
	runErr   error
	stopErr  error
	stopped  chan struct{}
	stopOnce sync.Once
}

func newFakeSCM() *fakeSCM {
	return &fakeSCM{stop: make(chan struct{}), started: make(chan struct{}), stopped: make(chan struct{})}
}

// Stop is the Service.Stop that the program calls when it ends by itself.
func (f *fakeSCM) requestStop() { f.stopOnce.Do(func() { close(f.stop) }) }

func (f *fakeSCM) Run() error {
	if f.runErr != nil {
		return f.runErr
	}
	if err := f.prog.Start(f.service()); err != nil {
		return err
	}
	close(f.started)
	<-f.stop
	f.stopErr = f.prog.Stop(f.service())
	close(f.stopped)
	return nil
}

func (f *fakeSCM) service() ksvc.Service { return stopper{f} }

// stopper is the ksvc.Service handed to Start; only Stop is used.
type stopper struct{ f *fakeSCM }

func (s stopper) Stop() error { s.f.requestStop(); return nil }

func (stopper) Run() error                                     { return nil }
func (stopper) Start() error                                   { return nil }
func (stopper) Restart() error                                 { return nil }
func (stopper) Install() error                                 { return nil }
func (stopper) Uninstall() error                               { return nil }
func (stopper) Logger(chan<- error) (ksvc.Logger, error)       { return nil, nil }
func (stopper) SystemLogger(chan<- error) (ksvc.Logger, error) { return nil, nil }
func (stopper) String() string                                 { return "fake" }
func (stopper) Platform() string                               { return "fake" }
func (stopper) Status() (ksvc.Status, error)                   { return ksvc.StatusUnknown, nil }

func (f *fakeSCM) scm() SCM {
	return SCM{
		GOOS:        "windows",
		Interactive: func() bool { return false },
		New: func(p ksvc.Interface, c *ksvc.Config) (ServiceRunner, error) {
			f.prog, f.cfg = p, c
			return f, nil
		},
		StopTimeout: 200 * time.Millisecond,
	}
}

func TestSCMRunPerformsTheServiceHandshake(t *testing.T) {
	f := newFakeSCM()
	running := make(chan context.Context, 1)
	done := make(chan error, 1)
	go func() {
		done <- f.scm().Run(context.Background(), `C:\minion.exe`, func(ctx context.Context) error {
			running <- ctx
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	var ctx context.Context
	select {
	case ctx = <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("the Worker was not started through the manager")
	}
	if f.cfg == nil || f.cfg.Name != WindowsServiceName {
		t.Fatalf("service config = %+v", f.cfg)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned before the manager asked to stop: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if ctx.Err() != nil {
		t.Fatal("the Worker was stopped before the stop request")
	}
	f.requestStop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v after a stop request", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the stop request")
	}
	if ctx.Err() == nil {
		t.Error("the stop request did not end the Worker")
	}
	if f.stopErr != nil {
		t.Errorf("Stop = %v", f.stopErr)
	}
}

func TestSCMRunStopsTheServiceWhenTheWorkerEndsWithoutAnError(t *testing.T) {
	f := newFakeSCM()
	err := f.scm().Run(context.Background(), "", func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	select {
	case <-f.stopped:
	default:
		t.Error("the manager was not told that the Worker ended")
	}
}

// A Worker that fails (an occupied pipe, a database that cannot be opened) must
// end the process like a crash, not report a clean stop: only then does the
// failure action of the service (restart) apply.
func TestSCMRunExitsAbnormallyWhenTheWorkerFails(t *testing.T) {
	f := newFakeSCM()
	boom := errors.New("cannot open the state database")
	codes := make(chan int, 1)
	s := f.scm()
	s.Exit = func(code int) { codes <- code; f.requestStop() }
	err := s.Run(context.Background(), "", func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the Worker error", err)
	}
	select {
	case code := <-codes:
		if code == 0 {
			t.Errorf("exit code = %d, want non-zero", code)
		}
	default:
		t.Fatal("the process did not exit abnormally")
	}
}

func TestSCMRunNeverExitsAbnormallyOnAnOperatorStop(t *testing.T) {
	f := newFakeSCM()
	s := f.scm()
	s.Exit = func(code int) { t.Errorf("exit(%d) on an operator stop", code) }
	running := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Run(context.Background(), "", func(ctx context.Context) error {
			close(running)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-running
	f.requestStop()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestSCMRunReportsAWorkerThatDoesNotStop(t *testing.T) {
	f := newFakeSCM()
	release := make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		done <- f.scm().Run(context.Background(), "", func(context.Context) error { <-release; return nil })
	}()
	<-f.started
	f.requestStop()
	<-f.stopped
	if f.stopErr == nil || !strings.Contains(f.stopErr.Error(), "did not stop") {
		t.Errorf("Stop = %v", f.stopErr)
	}
}

func TestSCMRunReportsAFailedHandshake(t *testing.T) {
	f := newFakeSCM()
	f.runErr = errors.New("not a service")
	err := f.scm().Run(context.Background(), "", func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not a service") {
		t.Fatalf("Run = %v", err)
	}
}

func TestSCMRunRunsDirectlyInAConsoleAndOffWindows(t *testing.T) {
	for name, s := range map[string]SCM{
		"console on windows": {GOOS: "windows", Interactive: func() bool { return true }},
		"linux":              {GOOS: "linux", Interactive: func() bool { return false }},
		"darwin":             {GOOS: "darwin"},
	} {
		s.New = func(ksvc.Interface, *ksvc.Config) (ServiceRunner, error) {
			t.Errorf("%s: the service manager was used", name)
			return nil, errors.New("unexpected")
		}
		want := errors.New("from fn")
		if err := s.Run(context.Background(), "", func(context.Context) error { return want }); !errors.Is(err, want) {
			t.Errorf("%s: Run = %v", name, err)
		}
	}
}
