package cli

// minion worker start: starts the Worker that "minion worker up" installed
// and "minion worker down" stopped, through the same manager that supervises
// it (the logon supervisor of Windows, the systemd unit, the launchd agent).
// It installs and changes nothing: a Worker that runs is left alone, and
// without an installed Worker the command points to "minion worker up".

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerStartCmd(defaultWorkerStartDeps()))
	})
}

// errNotInstalled says there is nothing to start.
var errNotInstalled = errors.New("no Worker is installed for this user; set it up with: minion worker up")

// workerStartDeps are the seams of "worker start" that tests replace.
type workerStartDeps struct {
	goos   string
	run    func(ctx context.Context, name string, args ...string) error
	output func(ctx context.Context, name string, args ...string) (string, error)
	// autostart reads the Run entry of the Worker (Windows); supervisorRunning
	// and startLauncher are the supervisor of the logon Worker (Windows).
	autostart         func() (string, bool, error)
	supervisorRunning func() bool
	startLauncher     func(exe string, args []string) error
	// call is the local control socket of this user.
	call       func(ctx context.Context, op string) error
	fileExists func(path string) bool
	home       func() string
	uid        func() int
	euid       func() int
	user       func() string
	sleep      func(time.Duration)
	// polls bounds the wait for the started Worker in steps of 250 ms.
	polls int
}

func defaultWorkerStartDeps() workerStartDeps {
	return workerStartDeps{
		goos: runtime.GOOS,
		run: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		},
		output: func(ctx context.Context, name string, args ...string) (string, error) {
			b, err := exec.CommandContext(ctx, name, args...).Output()
			return string(b), err
		},
		autostart:         service.Autostart,
		supervisorRunning: service.SupervisorRunning,
		startLauncher:     startLauncher,
		call: func(ctx context.Context, op string) error {
			_, err := client.Local().Call(ctx, op, nil)
			return err
		},
		fileExists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		home: func() string {
			h, _ := os.UserHomeDir()
			return h
		},
		uid:  os.Getuid,
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

func newWorkerStartCmd(d workerStartDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the installed Worker of this user (the opposite of worker down)",
		Long: "Start the Worker that \"minion worker up\" installed, through its own manager: the logon supervisor on\n" +
			"Windows, the systemd unit on Linux, the launchd agent on macOS. A Worker that already runs is left\n" +
			"alone. Nothing is installed or changed; without an installed Worker the command says so and points to\n" +
			"\"minion worker up\".",
		Args: cobra.NoArgs,
		RunE: withHubLine(func(cmd *cobra.Command, _ []string) error { return runWorkerStart(cmd, d) }, nil),
	}
}

func (d workerStartDeps) live(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return d.call(cctx, agent.OpLocalStatus) == nil
}

func runWorkerStart(cmd *cobra.Command, d workerStartDeps) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	if d.live(ctx) {
		_, _ = fmt.Fprintln(out, "The Worker is already running.")
		return nil
	}
	started, err := d.startManaged(ctx)
	if err != nil {
		return err
	}
	if !started {
		_, _ = fmt.Fprintln(out, "The Worker is already running.")
		return nil
	}
	polls := d.polls
	if polls == 0 {
		polls = 120
	}
	for i := 0; i < polls; i++ {
		if d.live(ctx) {
			_, _ = fmt.Fprintln(out, "The Worker started.")
			return nil
		}
		d.sleep(250 * time.Millisecond)
	}
	return errors.New("the Worker was started but does not answer yet; check it with: minion worker status")
}

// startManaged starts the installed Worker; started is false when its manager
// reports that it runs already.
func (d workerStartDeps) startManaged(ctx context.Context) (started bool, err error) {
	switch d.goos {
	case "windows":
		return d.startWindows()
	case "linux":
		return d.startSystemd(ctx)
	case "darwin":
		return d.startLaunchd(ctx)
	}
	return false, errNotInstalled
}

func (d workerStartDeps) startWindows() (bool, error) {
	cmdline, ok, err := d.autostart()
	if err != nil {
		return false, fmt.Errorf("cannot read the Worker's Run entry: %w", err)
	}
	if !ok {
		return false, errNotInstalled
	}
	if d.supervisorRunning() {
		return false, nil
	}
	parts := splitWindowsCommand(cmdline)
	if len(parts) < 3 || parts[1] != "worker" || parts[2] != service.LaunchCommand {
		return false, errors.New("the Run entry " + service.AutostartValueName + " is not the Worker supervisor; set the Worker up again with: minion worker up")
	}
	if err := d.startLauncher(parts[0], parts[3:]); err != nil {
		return false, fmt.Errorf("cannot start the Worker: %w", err)
	}
	return true, nil
}

func (d workerStartDeps) startSystemd(ctx context.Context) (bool, error) {
	if d.unitLoaded(ctx, "--user", service.UserUnitName) {
		if unitRunning(ctx, workerDownDeps{output: d.output}, "--user", service.UserUnitName) {
			return false, nil
		}
		if err := d.run(ctx, "systemctl", "--user", "start", service.UserUnitName); err != nil {
			return false, fmt.Errorf("cannot start the user service %s (%v); run: systemctl --user start %s", service.UserUnitName, err, service.UserUnitName)
		}
		return true, nil
	}
	unit := service.SystemUnitFor(d.user())
	if d.unitLoaded(ctx, "", unit) {
		if unitRunning(ctx, workerDownDeps{output: d.output}, "", unit) {
			return false, nil
		}
		if d.euid() != 0 {
			return false, fmt.Errorf("the Worker is the system service %s; start it with:\n  sudo systemctl start %s", unit, unit)
		}
		if err := d.run(ctx, "systemctl", "start", unit); err != nil {
			return false, fmt.Errorf("cannot start %s (%v); run: systemctl start %s", unit, err, unit)
		}
		return true, nil
	}
	return false, errNotInstalled
}

func (d workerStartDeps) unitLoaded(ctx context.Context, scope, unit string) bool {
	args := []string{"show", "-p", "LoadState", "--value", unit}
	if scope != "" {
		args = append([]string{scope}, args...)
	}
	out, err := d.output(ctx, "systemctl", args...)
	return err == nil && strings.TrimSpace(out) == "loaded"
}

func (d workerStartDeps) startLaunchd(ctx context.Context) (bool, error) {
	plist := service.PlistPath(d.home())
	if !d.fileExists(plist) {
		return false, errNotInstalled
	}
	domain := "gui/" + strconv.Itoa(d.uid())
	target := domain + "/" + service.LaunchdLabel
	// "worker down" boots the agent out: load it again before it is started.
	if d.run(ctx, "launchctl", "print", target) != nil {
		if err := d.run(ctx, "launchctl", "bootstrap", domain, plist); err != nil {
			return false, fmt.Errorf("cannot load the launchd agent (%v); run: launchctl bootstrap %s %s", err, domain, plist)
		}
	}
	// kickstart without -k leaves a running agent alone.
	if err := d.run(ctx, "launchctl", "kickstart", target); err != nil {
		return false, fmt.Errorf("cannot start the launchd agent (%v); run: launchctl kickstart %s", err, target)
	}
	return true, nil
}

// splitWindowsCommand splits a command line the way the Run key writes it
// (see service.AutostartCommand): words separated by blanks, a word in double
// quotes may hold blanks and \" for a quote.
func splitWindowsCommand(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == '"':
			cur.WriteByte('"')
			have = true
			i++
		case c == '"':
			inQuote, have = !inQuote, true
		case (c == ' ' || c == '\t') && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}
