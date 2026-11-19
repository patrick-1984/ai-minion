package cli

// minion worker down (v0.9.3, A27): stops the Worker of this user, the
// opposite of "worker up". A Worker run by a service manager is stopped
// through that manager (systemd, launchd, the Windows service control
// manager), because it would restart a Worker that simply exits; a foreground
// Worker is asked to stop over the local control socket. When stopping needs
// rights this process does not have, the exact command is printed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
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
	// output runs a command and returns its standard output.
	output func(ctx context.Context, name string, args ...string) (string, error)
	// windowsService stops the Windows service when it runs: stopped reports
	// whether it ran and was stopped.
	windowsService func() (stopped bool, err error)
	// call is the local control socket of this user; callArgs is the same
	// with arguments (nil: call with no arguments).
	call     func(ctx context.Context, op string) (json.RawMessage, error)
	callArgs func(ctx context.Context, op string, args any) (json.RawMessage, error)
	// windowsRunning reports whether the Windows service runs (query only).
	windowsRunning func() bool
	euid           func() int
	user           func() string
	sleep          func(time.Duration)
	// stateFree reports whether the state lock in dir can be taken, that is
	// whether the stopped Worker has released everything ("" = this user's
	// default state directory).
	stateFree func(dir string) bool
}

func defaultWorkerDownDeps() workerDownDeps {
	return workerDownDeps{
		goos: runtime.GOOS,
		run: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		},
		output: func(ctx context.Context, name string, args ...string) (string, error) {
			b, err := exec.CommandContext(ctx, name, args...).Output()
			return string(b), err
		},
		windowsService: stopWindowsService,
		call: func(ctx context.Context, op string) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, nil)
		},
		callArgs: func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		},
		windowsRunning: windowsServiceRunning,
		euid:           os.Geteuid,
		user: func() string {
			if u, err := user.Current(); err == nil {
				return u.Username
			}
			return os.Getenv("USER")
		},
		sleep:     time.Sleep,
		stateFree: workerStateFree,
	}
}

// workerStateFree takes and releases the state lock in dir (the running
// Worker's own state directory, else this user's default one).
func workerStateFree(dir string) bool {
	if dir == "" {
		res := &config.Resolver{Registry: config.Default, Role: config.ScopeWorker, LookupEnv: os.LookupEnv}
		r, err := res.Resolve(config.KeyStateDir)
		if err != nil || r.Value == "" {
			return true
		}
		dir = r.Value
	}
	l, err := agent.LockLifecycle(dir)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// windowsServiceRunning reports whether the Windows service runs (query
// only; false when unknown). It asks with query rights only: the status of
// the kardianos controller opens the service with start and stop rights too,
// which a standard user does not have, and would report "not running" for a
// service that runs (windows.yml, principal b).
func windowsServiceRunning() bool {
	st, err := service.QueryWindowsService()
	return err == nil && st.Running
}

// stopWindowsService stops the Windows service when it runs. Whether it runs
// is asked with query rights only, so that a user who may not stop it gets the
// access-denied error of the stop (exit 21) instead of "not running".
func stopWindowsService() (bool, error) {
	st, err := service.QueryWindowsService()
	if err != nil || !st.Running {
		return false, nil // not installed, not running, or not readable
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	c, err := service.NewKardianosController(exe)
	if err != nil {
		return false, err
	}
	stopper, ok := c.(interface{ Stop() error })
	if !ok {
		return false, errors.New("the service controller cannot stop services")
	}
	return true, stopper.Stop()
}

func newWorkerDownCmd(d workerDownDeps) *cobra.Command {
	var o downOptions
	cmd := &cobra.Command{
		Use:     "down",
		Aliases: []string{"stop"},
		Short:   "Stop the running Worker of this user (the opposite of worker up)",
		Long: "Stop the running Worker of this user. A Worker run as a service is stopped through its service\n" +
			"manager (systemd, launchd, Windows services), a foreground Worker over its control socket.\n" +
			"Its sessions keep running (tmux, psmux); a service starts again at the next boot or login.\n\n" +
			"Sessions that live inside the Worker (Windows ConPTY) end with it. The running Worker counts them\n" +
			"first: when any would end the command exits 23 with sessions_lost N and stops nothing; repeat it\n" +
			"with --accept-loss N. When they cannot be counted (an older Worker, a Worker that does not answer)\n" +
			"it exits 29; --accept-unknown-loss accepts that an unknown number of sessions will end.\n" +
			"--scm-only stops the Windows service through the service control manager only (needs\n" +
			"--accept-unknown-loss and an elevated prompt). --state-dir D stops only a Worker that answers\n" +
			"for that state directory: a Worker of another one is left running and the command exits 27\n" +
			"(the tray passes its profile this way). --json prints one JSON result object.",
		Args: cobra.NoArgs,
		RunE: withHubLine(func(cmd *cobra.Command, _ []string) error {
			dir, err := explicitStateDir(cmd)
			if err != nil {
				return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), o.json, downResult{}, err)
			}
			o.stateDir = dir
			res, err := runWorkerDownOpts(cmd, d, o)
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), o.json, res, err)
		}, nil),
	}
	cmd.Flags().IntVar(&o.acceptLoss, "accept-loss", 0, "accept that up to this many sessions end (ConPTY)")
	cmd.Flags().BoolVar(&o.acceptUnknown, "accept-unknown-loss", false, "accept that an unknown number of sessions end")
	cmd.Flags().BoolVar(&o.scmOnly, "scm-only", false, "Windows: stop the service through the service control manager only")
	cmd.Flags().BoolVar(&o.json, "json", false, "print one JSON result object")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

// downOptions are the flags of "worker down".
type downOptions struct {
	acceptLoss    int
	acceptUnknown bool
	scmOnly       bool
	json          bool
	// stateDir binds the stop to the Worker of this state directory ("":
	// whichever Worker of this user answers).
	stateDir string
}

// downResult is the JSON result of "worker down".
type downResult struct {
	Stopped      bool `json:"stopped"`
	SessionsLost *int `json:"sessions_lost,omitempty"`
}

func runWorkerDown(cmd *cobra.Command, d workerDownDeps) error {
	_, err := runWorkerDownOpts(cmd, d, downOptions{})
	return err
}

// hostKeepsSessions reports whether the sessions of host outlive the Worker
// (tmux, psmux; on Linux and macOS always tmux).
func hostKeepsSessions(goos, host string) bool {
	return host == "tmux" || host == "psmux" || (host == "" && goos != "windows")
}

func runWorkerDownOpts(cmd *cobra.Command, d workerDownDeps, o downOptions) (downResult, error) {
	var res downResult
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	if o.json {
		out = io.Discard
	}
	if o.scmOnly {
		if !o.acceptUnknown {
			return res, &usageError{errors.New("--scm-only stops without counting the sessions: add --accept-unknown-loss")}
		}
		stopped, err := stopServiceWorker(ctx, out, d, o.stateDir)
		res.Stopped = stopped
		return res, err
	}
	// Ask the running Worker first: it counts the sessions that would end.
	var reply agent.LiveStatusReply
	live := false
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if d.call != nil {
		if raw, serr := d.call(cctx, agent.OpLocalStatus); serr == nil && json.Unmarshal(raw, &reply) == nil {
			live = true
		}
	}
	cancel()
	if live && o.stateDir != "" && !sameDir(reply.StateDir, o.stateDir) {
		return res, otherProfileError("stopped", reply.StateDir, o.stateDir)
	}
	if live && d.goos == "linux" {
		// Refuse before the Worker takes its create hold: a system unit that
		// this user may not stop would leave the hold in place.
		if err := stopPermitted(ctx, d); err != nil {
			return res, err
		}
	}
	if live && d.callArgs != nil {
		args := agent.LocalLifecycleArgs{Action: agent.LifecycleStop, Instance: reply.Instance, AcceptLoss: o.acceptLoss, AcceptUnknown: o.acceptUnknown}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		raw, err := d.callArgs(cctx, localapi.QualifiedOp(agent.OpLocalLifecycle, []string{agent.FeatureLifecycle}), args)
		cancel()
		if err == nil {
			var lr agent.LifecycleResult
			if json.Unmarshal(raw, &lr) != nil {
				return res, errors.New("the running Worker returned an invalid lifecycle result")
			}
			if lr.SessionsLost >= 0 {
				n := lr.SessionsLost
				res.SessionsLost = &n
			}
			if !lr.Exiting {
				// systemd or launchd: stop the unit, or it starts again.
				stopped, err := stopServiceWorker(ctx, out, d, reply.StateDir)
				res.Stopped = stopped
				return res, err
			}
			return res, waitStopped(ctx, out, d, reply.StateDir, &res)
		}
		if rerr := lifecycleRefusal(err); rerr != nil {
			return res, rerr
		}
		var pe *proto.Error
		tooOld := errors.Is(err, client.ErrWorkerTooOld) || (errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp)
		if !tooOld {
			return res, fmt.Errorf("cannot ask the running Worker to stop: %w", err)
		}
		// An older Worker: its bare worker_stop ignores any count, so it is
		// sent only where no session can end.
		if !hostKeepsSessions(d.goos, reply.SessionHost) && !o.acceptUnknown {
			return res, &resultError{Exit: ExitCountUnavailable, Message: "the running Worker is too old to count the sessions that would end; " +
				"nothing was stopped. Repeat with --accept-unknown-loss to accept that an unknown number of sessions will end"}
		}
	} else if !live && d.goos == "windows" && d.windowsRunning != nil && d.windowsRunning() && !o.acceptUnknown {
		return res, &resultError{Exit: ExitCountUnavailable, Message: "the Windows service runs but does not answer on this user's control socket, " +
			"so the sessions that would end cannot be counted; nothing was stopped. Repeat with --accept-unknown-loss to accept that an unknown number of sessions will end"}
	}
	return res, legacyWorkerDown(ctx, out, d, o, &res)
}

// lifecycleRefusal maps a refusal of worker_lifecycle to its exit status, or
// returns nil for another error.
func lifecycleRefusal(err error) error {
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict {
		return nil
	}
	var det struct {
		Reason       string `json:"reason"`
		SessionsLost int    `json:"sessions_lost"`
	}
	_ = json.Unmarshal(pe.Details, &det)
	switch det.Reason {
	case agent.ReasonSessionsWouldEnd:
		return &resultError{Exit: ExitSessionsWouldEnd, Message: pe.Message + "; repeat with --accept-loss " + strconv.Itoa(det.SessionsLost) + " to accept that",
			Fields: map[string]any{"sessions_lost": det.SessionsLost}}
	case agent.ReasonCountUnavailable:
		return &resultError{Exit: ExitCountUnavailable, Message: pe.Message + "; repeat with --accept-unknown-loss to accept that an unknown number of sessions will end"}
	case "state_changed":
		return &resultError{Exit: ExitStateChanged, Message: pe.Message}
	}
	return nil
}

// legacyWorkerDown is the stop without a count: through the service manager,
// else the bare worker_stop of the running Worker.
func legacyWorkerDown(ctx context.Context, out io.Writer, d workerDownDeps, o downOptions, res *downResult) error {
	stoppedService, err := stopServiceWorker(ctx, out, d, o.stateDir)
	if err != nil {
		return err
	}
	if stoppedService {
		res.Stopped = true
		return nil
	}
	// A foreground Worker (or one no service manager runs).
	// The running Worker names its own state directory (it may be another one
	// than this user's default, --state-dir): that is the lock to wait for.
	stateDir := ""
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if raw, serr := d.call(cctx, agent.OpLocalStatus); serr == nil {
		var reply agent.LiveStatusReply
		if json.Unmarshal(raw, &reply) == nil {
			stateDir = reply.StateDir
			// A Worker that started answering since the first probe was
			// not counted: verify its profile and its session host first.
			if o.stateDir != "" && !sameDir(reply.StateDir, o.stateDir) {
				cancel()
				return otherProfileError("stopped", reply.StateDir, o.stateDir)
			}
			if !hostKeepsSessions(d.goos, reply.SessionHost) && !o.acceptUnknown {
				cancel()
				return &resultError{Exit: ExitCountUnavailable, Message: "a Worker started answering after the sessions were counted, so the sessions that would end " +
					"cannot be counted; nothing was stopped. Repeat with --accept-unknown-loss to accept that an unknown number of sessions will end"}
			}
		}
	}
	_, err = d.call(cctx, agent.OpLocalStop)
	cancel()
	switch {
	case errors.Is(err, client.ErrWorkerNotRunning):
		_, _ = fmt.Fprintln(out, "No Worker is running for this user.")
		return nil
	case err != nil:
		return fmt.Errorf("cannot ask the running Worker to stop: %w", err)
	}
	return waitStopped(ctx, out, d, stateDir, res)
}

// waitStopped waits until the Worker answers no more and its state lock is
// free (the connector, the pipe and the store have finished), so that "worker
// reset" or "worker up" works right after (up to 30 seconds).
func waitStopped(ctx context.Context, out io.Writer, d workerDownDeps, stateDir string, res *downResult) error {
	for i := 0; i < 120; i++ {
		d.sleep(250 * time.Millisecond)
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := d.call(cctx, agent.OpLocalStatus)
		cancel()
		if errors.Is(err, client.ErrWorkerNotRunning) && d.stateFree(stateDir) {
			res.Stopped = true
			_, _ = fmt.Fprintln(out, "The Worker stopped. Start it again with: minion worker start (a service) or minion worker up --foreground")
			return nil
		}
	}
	return errors.New("the Worker was asked to stop but has not finished after 30 seconds; check it with: minion worker status")
}

// stopServiceWorker stops the Worker service of this user when one runs. It
// reports whether it stopped one; an error carries the command to run when
// this process lacks the rights.
func stopServiceWorker(ctx context.Context, out io.Writer, d workerDownDeps, wantDir string) (bool, error) {
	switch d.goos {
	case "windows":
		stopped, err := d.windowsService()
		if err != nil {
			return false, &resultError{Exit: ExitNeedsAdmin, Message: fmt.Sprintf("the Worker runs as the Windows service %s and could not be stopped (%v); "+
				"in a terminal opened with \"Run as administrator\" run: minion worker down   (or: Stop-Service %s)",
				service.WindowsServiceName, err, service.WindowsServiceName)}
		}
		if stopped {
			_, _ = fmt.Fprintf(out, "Stopped the Windows service %s. It starts again at the next boot; start it now with: Start-Service %s\n",
				service.WindowsServiceName, service.WindowsServiceName)
		}
		return stopped, nil
	case "linux":
		if unitRunning(ctx, d, "--user", service.UserUnitName) {
			if err := unitProfileMatches(ctx, d, "--user", service.UserUnitName, wantDir); err != nil {
				return false, err
			}
			if err := d.run(ctx, "systemctl", "--user", "stop", service.UserUnitName); err != nil {
				return false, fmt.Errorf("cannot stop the user service %s (%v); run: systemctl --user stop %s", service.UserUnitName, err, service.UserUnitName)
			}
			_, _ = fmt.Fprintf(out, "Stopped the user service %s. Start it again with: systemctl --user start %s\n", service.UserUnitName, service.UserUnitName)
			return true, nil
		}
		unit := service.SystemUnitFor(d.user())
		if unitRunning(ctx, d, "", unit) {
			if err := unitProfileMatches(ctx, d, "", unit, wantDir); err != nil {
				return false, err
			}
			if d.euid() != 0 {
				return false, &resultError{Exit: ExitNeedsAdmin, Message: fmt.Sprintf("the Worker runs as the system service %s; stop it with:\n  sudo systemctl stop %s\n"+
					"(and keep it from starting at boot with: sudo systemctl disable %s)", unit, unit, unit)}
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

// unitRunning reports whether a systemd unit runs or is about to (active,
// activating — also the wait before an automatic restart — or reloading).
func unitRunning(ctx context.Context, d workerDownDeps, scope, unit string) bool {
	args := []string{"show", "-p", "ActiveState", "--value", unit}
	if scope != "" {
		args = append([]string{scope}, args...)
	}
	out, err := d.output(ctx, "systemctl", args...)
	if err != nil {
		return false
	}
	switch strings.TrimSpace(out) {
	case "active", "activating", "reloading":
		return true
	}
	return false
}

// stopPermitted fails with exit 21 when the only running unit is a system
// unit that this user may not stop, before anything is asked of the Worker.
func stopPermitted(ctx context.Context, d workerDownDeps) error {
	if d.euid == nil || d.euid() == 0 || d.output == nil {
		return nil
	}
	if unitRunning(ctx, d, "--user", service.UserUnitName) {
		return nil
	}
	unit := service.SystemUnitFor(d.user())
	if unitRunning(ctx, d, "", unit) {
		return &resultError{Exit: ExitNeedsAdmin, Message: fmt.Sprintf("the Worker runs as the system service %s; stop it with:\n  sudo systemctl stop %s\n"+
			"(and keep it from starting at boot with: sudo systemctl disable %s); nothing was stopped", unit, unit, unit)}
	}
	return nil
}

// unitProfileMatches checks that the systemd unit runs the Worker of want
// (the MINION_STATE_DIR it pins, else the default state directory). want ""
// asks for no check.
func unitProfileMatches(ctx context.Context, d workerDownDeps, scope, unit, want string) error {
	if want == "" {
		return nil
	}
	args := []string{"show", "-p", "Environment", "--value", unit}
	if scope != "" {
		args = append([]string{scope}, args...)
	}
	out, err := d.output(ctx, "systemctl", args...)
	if err != nil {
		return nil // unreadable: the unit's own refusal rules apply
	}
	got := defaultStateDirOf()
	for _, kv := range strings.Fields(out) {
		if v, ok := strings.CutPrefix(kv, "MINION_STATE_DIR="); ok {
			got = strings.Trim(v, `"`)
		}
	}
	if got != "" && !sameDir(got, want) {
		return otherProfileError("stopped", got, want)
	}
	return nil
}

// defaultStateDirOf is the state directory a Worker uses with no pin.
func defaultStateDirOf() string {
	res := &config.Resolver{Registry: config.Default, Role: config.ScopeWorker, LookupEnv: func(string) (string, bool) { return "", false }}
	r, err := res.Resolve(config.KeyStateDir)
	if err != nil {
		return ""
	}
	return r.Value
}
