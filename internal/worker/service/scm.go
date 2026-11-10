package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	ksvc "github.com/kardianos/service"
)

// SCM carries what [SCM.Run] reads from the machine; tests replace it. The
// zero value is the real machine.
type SCM struct {
	// GOOS is the operating system; empty means runtime.GOOS.
	GOOS string
	// Interactive reports a process started from a console, not by the
	// service control manager; nil means kardianos' Interactive.
	Interactive func() bool
	// New creates the kardianos service of prog; nil means ksvc.New.
	New func(prog ksvc.Interface, cfg *ksvc.Config) (ServiceRunner, error)
	// StopTimeout bounds the wait for run to return after a stop request; zero
	// means 20 seconds, inside the 30 seconds that Windows gives a service.
	StopTimeout time.Duration
	// Exit ends the process abnormally, without reporting the service as
	// stopped; nil means os.Exit. The manager applies the failure actions of
	// the service (restart) only to a process that ends that way: a service
	// that reports itself stopped, even with an error, is not restarted.
	Exit func(code int)
}

// ServiceRunner is the part of a kardianos service that hands the process to
// the service control manager.
type ServiceRunner interface {
	// Run connects to the manager, calls the program's Start, blocks until
	// the manager asks to stop and returns after the program's Stop.
	Run() error
}

func (s SCM) goos() string {
	if s.GOOS != "" {
		return s.GOOS
	}
	return runtime.GOOS
}

func (s SCM) interactive() bool {
	if s.Interactive != nil {
		return s.Interactive()
	}
	return ksvc.Interactive()
}

func (s SCM) newService(prog ksvc.Interface, cfg *ksvc.Config) (ServiceRunner, error) {
	if s.New != nil {
		return s.New(prog, cfg)
	}
	return ksvc.New(prog, cfg)
}

func (s SCM) exit(code int) {
	if s.Exit != nil {
		s.Exit(code)
		return
	}
	os.Exit(code)
}

func (s SCM) stopTimeout() time.Duration {
	if s.StopTimeout > 0 {
		return s.StopTimeout
	}
	return 20 * time.Second
}

// Run runs the Worker. On Windows, in a process that the service control
// manager started, it performs the service handshake: it reports the service
// as started, runs fn until the manager asks to stop, cancels fn's context on
// that request and reports the service as stopped once fn has returned. Without
// the handshake the manager gives up on the service after 30 seconds.
// Everywhere else, and in a console on Windows, it runs fn directly.
//
// fn must return when its context ends. When fn ends by itself without an
// error, the service stops and Run returns nil. When fn ends by itself with an
// error (the state database cannot be opened, the control pipe is taken), the
// error is written to the Windows event log and the process exits with code 1
// without reporting the service as stopped, so that the failure actions of the
// service (restart after 5 seconds, see [WindowsConfig]) apply. An operator
// stop never restarts the service.
func (s SCM) Run(ctx context.Context, binary string, fn func(context.Context) error) error {
	if s.goos() != "windows" || s.interactive() {
		return fn(ctx)
	}
	prog := &scmProgram{parent: ctx, run: fn, timeout: s.stopTimeout(), exit: s.exit}
	svc, err := s.newService(prog, KardianosConfig(binary))
	if err != nil {
		return fmt.Errorf("service manager: %w", err)
	}
	if err := svc.Run(); err != nil {
		return errors.Join(fmt.Errorf("service control manager: %w", err), prog.result())
	}
	return prog.result()
}

// RunUnderSCM runs fn like [SCM.Run] does on the real machine.
func RunUnderSCM(ctx context.Context, fn func(context.Context) error) error {
	return SCM{}.Run(ctx, "", fn)
}

// scmProgram is the kardianos program of the Worker: Start launches the
// Worker in the background (it must return quickly) and Stop ends it.
type scmProgram struct {
	parent  context.Context
	run     func(context.Context) error
	timeout time.Duration
	exit    func(code int)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Start implements the kardianos Interface.
func (p *scmProgram) Start(s ksvc.Service) error {
	ctx, cancel := context.WithCancel(p.parent)
	done := make(chan struct{})
	p.mu.Lock()
	p.cancel, p.done = cancel, done
	p.mu.Unlock()
	go func() {
		err := p.run(ctx)
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		stopped := ctx.Err() != nil
		close(done)
		cancel()
		if stopped {
			return
		}
		// Ended by itself. A failure (for example the Worker could not
		// start) must end the process the way a crash does: reporting the
		// service as stopped, even with an error, would not trigger the
		// restart of the service. A plain end tells the manager to stop.
		if err != nil && !errors.Is(err, context.Canceled) {
			if s != nil {
				if l, lerr := s.Logger(nil); lerr == nil && l != nil {
					_ = l.Errorf("The Minion Worker failed and the service ends so that Windows restarts it: %v", err)
				}
			}
			p.exit(1)
			return
		}
		if s != nil {
			_ = s.Stop()
		}
	}()
	return nil
}

// Stop implements the kardianos Interface.
func (p *scmProgram) Stop(ksvc.Service) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	t := time.NewTimer(p.timeout)
	defer t.Stop()
	select {
	case <-done:
		return nil
	case <-t.C:
		return fmt.Errorf("the Worker did not stop within %s", p.timeout)
	}
}

// result returns the error of the Worker run, ignoring a plain cancellation:
// that is the stop request of the manager.
func (p *scmProgram) result() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if errors.Is(p.err, context.Canceled) {
		return nil
	}
	return p.err
}
