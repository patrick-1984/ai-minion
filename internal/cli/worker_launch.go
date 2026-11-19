package cli

// minion worker launch (security fix S1): the supervisor of the Windows
// Worker. The user's Run key starts it at logon (see
// service.AutostartCommand); it starts "minion worker run" as the user,
// without a console window, and starts it again when it ends with an error or
// asks for a restart (exit 75 after a self-update). A Worker that stops on
// request (worker down, exit 0) ends the supervisor too. This is the
// monitor-process model of Syncthing, without a Windows service.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerLaunchCmd())
	})
}

// launchLogName is the log of the supervisor and of the Worker it runs, in
// the state directory.
const launchLogName = "worker-launch.log"

// maxLaunchLog is the size above which the log is moved to .1 at start.
const maxLaunchLog = 5 << 20

func newWorkerLaunchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    service.LaunchCommand,
		Short:  "Run the Worker under its supervisor (started at logon on Windows)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := service.RefuseSystem(systemAccount()); err != nil {
				return err
			}
			res := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: os.LookupEnv}
			if file, err := loadConfigFile(res, loadWorkerConfig); err == nil {
				res.File = file
			}
			dir, err := res.Resolve(config.KeyStateDir)
			if err != nil {
				return err
			}
			// Everything after "launch" goes to "worker run" unchanged.
			return launchWorker(cmd.Context(), dir.Value, launchArgs(os.Args))
		},
	}
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}

// launchArgs returns the arguments after "worker launch".
func launchArgs(argv []string) []string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "worker" && argv[i+1] == service.LaunchCommand {
			return append([]string(nil), argv[i+2:]...)
		}
	}
	return nil
}

// openLaunchLog opens the log in dir for appending, after moving a large one
// to .1. The log holds the Worker's console output, which can include a
// pairing code, so it is created with a private DACL (the user and SYSTEM on
// Windows, 0600 elsewhere) and an existing log that is not private is
// replaced.
func openLaunchLog(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, launchLogName)
	// A retained copy keeps the ACL of the file it was: keep one only when
	// the log itself is private (checked on the open handle), and delete a
	// backup that is not. A readable backup that cannot be deleted stops the
	// start: pairing codes must not be written next to it.
	backup := p + ".1"
	if f, err := identity.OpenPrivate(p); err == nil {
		fi, serr := f.Stat()
		_ = f.Close()
		if serr == nil && fi.Size() > maxLaunchLog {
			if err := removeIfExists(backup); err != nil {
				return nil, backupError(backup, err)
			}
			_ = os.Rename(p, backup)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(p) // not private: never kept, never rotated
	}
	if f, err := identity.OpenPrivate(backup); err == nil {
		_ = f.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		if err := removeIfExists(backup); err != nil {
			return nil, backupError(backup, err)
		}
	}
	return identity.OpenPrivateAppend(p)
}

// launchEnv is the environment of the supervised Worker: the user's own
// environment without any MINION_ variable, then MINION_SUPERVISED=1. The
// Worker reads its settings from the config file that "worker up" saved (and
// from the flags of the Run entry), so a variable of the shell that started
// "worker up" (MINION_SHARE_EXISTING, a one-time invitation key) can neither
// widen nor change what the user chose, and the immediate start and the start
// at logon behave alike.
func launchEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "MINION_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, agent.SupervisedEnv+"=1")
}

// supervisor runs the Worker again and again (see [supervise]).
type supervisor struct {
	// run starts the Worker once and returns its exit code when it ends; an
	// error means it could not be started.
	run   func(ctx context.Context) (int, error)
	sleep func(ctx context.Context, d time.Duration)
	now   func() time.Time
	log   io.Writer
	// stop, when it is closed, is a request to end ("worker down"): pending
	// restarts are cancelled, a running Worker is asked to stop through
	// askStop (retried until it ends, since it may not serve its pipe yet) and
	// forced after stopGrace. It is listened to during start-up and back-off.
	stop    <-chan struct{}
	askStop func()
	// stopGrace bounds the wait for the Worker after a stop request; zero
	// means 30 seconds.
	stopGrace time.Duration
	// stopEvery is the pause between stop requests; zero means 500 ms.
	stopEvery time.Duration
}

// Restart delays: 5 s after the first failure, doubling up to one minute; a
// Worker that ran for a minute counts as healthy again.
const (
	firstRestartDelay = 5 * time.Second
	maxRestartDelay   = time.Minute
	healthyRun        = time.Minute
)

// supervise runs the Worker until it ends cleanly (exit 0: it was asked to
// stop) or ctx ends. A restart request (exit 75) starts it again at once,
// with the binary now at the same path; any other end starts it again after a
// growing delay.
func (s supervisor) supervise(ctx context.Context) {
	var stopping atomic.Bool
	sctx, scancel := context.WithCancel(ctx) // ends back-off sleeps
	defer scancel()
	runCtx, rcancel := context.WithCancel(ctx) // ends a Worker that ignores the request
	defer rcancel()
	if s.stop != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-s.stop:
			case <-done:
				return
			case <-ctx.Done():
				return
			}
			stopping.Store(true)
			scancel()
			grace, every := s.stopGrace, s.stopEvery
			if grace <= 0 {
				grace = 30 * time.Second
			}
			if every <= 0 {
				every = 500 * time.Millisecond
			}
			deadline := time.NewTimer(grace)
			defer deadline.Stop()
			tick := time.NewTicker(every)
			defer tick.Stop()
			for {
				if s.askStop != nil {
					s.askStop()
				}
				select {
				case <-done:
					return
				case <-deadline.C:
					rcancel()
					return
				case <-tick.C:
				}
			}
		}()
	}
	delay := firstRestartDelay
	for {
		if stopping.Load() {
			s.logf("stopped on request")
			return
		}
		started := s.now()
		code, err := s.run(runCtx)
		if ctx.Err() != nil {
			return
		}
		if stopping.Load() {
			s.logf("stopped on request")
			return
		}
		switch {
		case err != nil:
			s.logf("cannot start the Worker: %v", err)
		case code == 0:
			s.logf("the Worker stopped")
			return
		case code == agent.ExitRestart:
			s.logf("the Worker asked for a restart (update); starting it again")
			delay = firstRestartDelay
			continue
		default:
			s.logf("the Worker ended with exit status %d", code)
		}
		if s.now().Sub(started) >= healthyRun {
			delay = firstRestartDelay
		}
		s.logf("starting the Worker again in %s", delay)
		s.sleep(sctx, delay)
		if ctx.Err() != nil {
			return
		}
		if delay *= 2; delay > maxRestartDelay {
			delay = maxRestartDelay
		}
	}
}

func (s supervisor) logf(format string, a ...any) {
	if s.log != nil {
		_, _ = fmt.Fprintf(s.log, "%s minion worker launch: %s\n", s.now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// hasStateDirArg reports whether args already carry --state-dir.
func hasStateDirArg(args []string) bool {
	for _, a := range args {
		if a == "--state-dir" || strings.HasPrefix(a, "--state-dir=") {
			return true
		}
	}
	return false
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func backupError(p string, err error) error {
	return fmt.Errorf("the old log %s can be read by other accounts and cannot be deleted (%v); delete it, then start the Worker again", p, err)
}
