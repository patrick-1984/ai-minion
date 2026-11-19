//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// launchWorker is "minion worker launch" on Windows: it leaves the console
// window that Windows opened for it (a console program started from the Run
// key gets one), makes sure it is the only supervisor of this user's session,
// and supervises "minion worker run" started with CREATE_NO_WINDOW, the
// documented way to run a console program without a console window
// (https://learn.microsoft.com/windows/win32/procthread/process-creation-flags).
// The Worker's output goes to the log in the state directory.
func launchWorker(ctx context.Context, stateDir string, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := service.RefuseSystem(service.IsSystemAccount()); err != nil {
		return err
	}
	freeConsole()
	release, already, err := service.AcquireSupervisor()
	if err != nil {
		return err
	}
	if already {
		return nil // another supervisor runs the Worker of this user
	}
	defer release()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The stop event comes right after the mutex and before any I/O, so
	// "worker down" never sees a supervisor it cannot signal; it is closed
	// (deferred later, so it runs first) before the mutex is released.
	stop, closeStop, err := service.CreateSupervisorStopEvent()
	if err != nil {
		return err
	}
	defer closeStop()
	logf, err := openLaunchLog(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	// The Worker reads the settings of the state directory this supervisor
	// resolved, not an environment variable of whoever started it.
	if !hasStateDirArg(args) {
		args = append(args, "--state-dir", stateDir)
	}
	s := supervisor{
		stop: stop,
		askStop: func() {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = client.Local().Call(cctx, agent.OpLocalStop, nil)
		},
		run: func(ctx context.Context) (int, error) {
			cmd := exec.CommandContext(ctx, exe, append([]string{"worker", "run"}, args...)...)
			cmd.Env = launchEnv(os.Environ())
			cmd.Stdout, cmd.Stderr = logf, logf
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
			err := cmd.Run()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			if err != nil {
				return -1, err
			}
			return 0, nil
		},
		sleep: sleepCtx,
		now:   time.Now,
		log:   logf,
	}
	s.logf("supervising %s worker run", exe)
	s.supervise(ctx)
	return nil
}

// freeConsole detaches this process from its console; Windows closes a
// console window that no process uses any more.
func freeConsole() {
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
}

// startLauncher starts "exe worker launch args" now, without a console window
// and outside this command's console and job, the way the Run key would at
// logon.
func startLauncher(exe string, args []string) error {
	cmd := exec.Command(exe, append([]string{"worker", "launch"}, args...)...)
	flags := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB}
	if err := cmd.Start(); err != nil {
		// A job that does not allow breaking away refuses the flag.
		cmd = exec.Command(exe, append([]string{"worker", "launch"}, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
	}
	return cmd.Process.Release()
}
