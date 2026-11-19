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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
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
// to .1.
func openLaunchLog(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, launchLogName)
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxLaunchLog {
		_ = os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

// supervisor runs the Worker again and again (see [supervise]).
type supervisor struct {
	// run starts the Worker once and returns its exit code when it ends; an
	// error means it could not be started.
	run   func(ctx context.Context) (int, error)
	sleep func(ctx context.Context, d time.Duration)
	now   func() time.Time
	log   io.Writer
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
	delay := firstRestartDelay
	for {
		started := s.now()
		code, err := s.run(ctx)
		if ctx.Err() != nil {
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
		s.sleep(ctx, delay)
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
