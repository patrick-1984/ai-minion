package cli

// minion worker down (v0.9.3, A27): stops the Worker of this user, the
// opposite of "worker up". A Worker run by a service manager is stopped
// through that manager (systemd, launchd, the Windows service control
// manager), because it would restart a Worker that simply exits; a foreground
// Worker is asked to stop over the local control socket. When stopping needs
// rights this process does not have, the exact command is printed.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerDownCmd(defaultWorkerDownDeps()))
	})
}

// workerDownDeps are the seams of "worker down" that tests replace.
type workerDownDeps struct {
	goos string
	// run runs a service manager command and reports whether it succeeded.
	run func(ctx context.Context, name string, args ...string) error
	// windowsService stops the Windows service when it runs: stopped reports
	// whether it ran and was stopped.
	windowsService func() (stopped bool, err error)
	// call is the local control socket of this user.
	call  func(ctx context.Context, op string) error
	euid  func() int
	user  func() string
	sleep func(time.Duration)
}

func defaultWorkerDownDeps() workerDownDeps {
	return workerDownDeps{
		goos: runtime.GOOS,
		run: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		},
		windowsService: stopWindowsService,
		call: func(ctx context.Context, op string) error {
			_, err := client.Local().Call(ctx, op, nil)
			return err
		},
		euid: os.Geteuid,
		user: func() string {
			if u, err := user.Current(); err == nil {
				return u.Username
			}
			return os.Getenv("USER")
		},
		sleep: time.Sleep,
	}
}

func stopWindowsService() (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	c, err := service.NewKardianosController(exe)
	if err != nil {
		return false, err
	}
	st, err := c.Status()
	if err != nil || st != service.StatusRunning {
		return false, nil // not installed or not running
	}
	stopper, ok := c.(interface{ Stop() error })
	if !ok {
		return false, errors.New("the service controller cannot stop services")
	}
	return true, stopper.Stop()
}

func newWorkerDownCmd(d workerDownDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the running Worker of this user (the opposite of worker up)",
		Long: "Stop the running Worker of this user. A Worker run as a service is stopped through its service\n" +
			"manager (systemd, launchd, Windows services), a foreground Worker over its control socket.\n" +
			"Its sessions keep running (tmux, psmux); a service starts again at the next boot or login.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runWorkerDown(cmd, d) },
	}
}

func runWorkerDown(cmd *cobra.Command, d workerDownDeps) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	stoppedService, err := stopServiceWorker(ctx, out, d)
	if err != nil {
		return err
	}
	if stoppedService {
		return nil
	}
	// A foreground Worker (or one no service manager runs).
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = d.call(cctx, agent.OpLocalStop)
	cancel()
	switch {
	case errors.Is(err, client.ErrWorkerNotRunning):
		_, _ = fmt.Fprintln(out, "No Worker is running for this user.")
		return nil
	case err != nil:
		return fmt.Errorf("cannot ask the running Worker to stop: %w", err)
	}
	for i := 0; i < 40; i++ {
		d.sleep(250 * time.Millisecond)
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.call(cctx, agent.OpLocalStatus)
		cancel()
		if errors.Is(err, client.ErrWorkerNotRunning) {
			_, _ = fmt.Fprintln(out, "The Worker stopped. Start it again with: minion worker up --foreground")
			return nil
		}
	}
	return errors.New("the Worker was asked to stop but still answers after 10 seconds; check it with: minion worker status")
}

// stopServiceWorker stops the Worker service of this user when one runs. It
// reports whether it stopped one; an error carries the command to run when
// this process lacks the rights.
func stopServiceWorker(ctx context.Context, out io.Writer, d workerDownDeps) (bool, error) {
	switch d.goos {
	case "windows":
		stopped, err := d.windowsService()
		if err != nil {
			return false, fmt.Errorf("the Worker runs as the Windows service %s and could not be stopped (%v); "+
				"in a terminal opened with \"Run as administrator\" run: minion worker down   (or: Stop-Service %s)",
				service.WindowsServiceName, err, service.WindowsServiceName)
		}
		if stopped {
			_, _ = fmt.Fprintf(out, "Stopped the Windows service %s. It starts again at the next boot; start it now with: Start-Service %s\n",
				service.WindowsServiceName, service.WindowsServiceName)
		}
		return stopped, nil
	case "linux":
		if d.run(ctx, "systemctl", "--user", "is-active", "--quiet", service.UserUnitName) == nil {
			if err := d.run(ctx, "systemctl", "--user", "stop", service.UserUnitName); err != nil {
				return false, fmt.Errorf("cannot stop the user service %s (%v); run: systemctl --user stop %s", service.UserUnitName, err, service.UserUnitName)
			}
			_, _ = fmt.Fprintf(out, "Stopped the user service %s. Start it again with: systemctl --user start %s\n", service.UserUnitName, service.UserUnitName)
			return true, nil
		}
		unit := service.SystemUnitFor(d.user())
		if d.run(ctx, "systemctl", "is-active", "--quiet", unit) == nil {
			if d.euid() != 0 {
				return false, fmt.Errorf("the Worker runs as the system service %s; stop it with:\n  sudo systemctl stop %s\n"+
					"(and keep it from starting at boot with: sudo systemctl disable %s)", unit, unit, unit)
			}
			if err := d.run(ctx, "systemctl", "stop", unit); err != nil {
				return false, fmt.Errorf("cannot stop %s (%v); run: systemctl stop %s", unit, err, unit)
			}
			_, _ = fmt.Fprintf(out, "Stopped the system service %s. Start it again with: sudo systemctl start %s\n", unit, unit)
			return true, nil
		}
	case "darwin":
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + service.LaunchdLabel
		if d.run(ctx, "launchctl", "print", target) == nil {
			if err := d.run(ctx, "launchctl", "bootout", target); err != nil {
				return false, fmt.Errorf("cannot stop the launchd agent (%v); run: launchctl bootout %s", err, target)
			}
			_, _ = fmt.Fprintf(out, "Stopped the launchd agent %s. It starts again at the next login; start it now with: minion worker up\n", service.LaunchdLabel)
			return true, nil
		}
	}
	return false, nil
}
