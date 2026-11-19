package cli

// "minion worker up" on Windows (security fix S1): the Worker runs as the
// user, started at logon from the user's Run key by its supervisor (minion
// worker launch), never as a Windows service. An older LocalSystem service is
// removed first (it needs an administrator; an elevated worker up does it),
// and the Worker is never installed from an elevated prompt, because it would
// then run with the elevated token.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// windowsUpDeps are the seams of the Windows path of "worker up".
type windowsUpDeps struct {
	query     func() (service.WindowsServiceState, error)
	elevation func() service.Elevation
	// isSystem reports whether this process is LocalSystem (nil: the token).
	isSystem func() bool
	// supervisorRunning reports whether a supervisor of this session runs;
	// signalStop asks it to stop (nil: the real ones).
	supervisorRunning func() bool
	// signalSupervisor asks the supervisor of this user to stop (nil: the
	// real one); found is false when none runs.
	signalSupervisor func() (found bool, err error)
	removeLegacy     func(ctx context.Context) error
	setAutostart     func(cmd string) error
	// call is the local control pipe of this user.
	call          func(ctx context.Context, op string) (json.RawMessage, error)
	startLauncher func(exe string, args []string) error
	sleep         func(time.Duration)
	// polls bounds each wait (stop, start) in steps of 250 ms; 0 means 120.
	polls int
}

func defaultWindowsUpDeps() windowsUpDeps {
	return windowsUpDeps{
		query:             service.QueryWindowsService,
		elevation:         service.CurrentElevation,
		isSystem:          systemAccount,
		supervisorRunning: service.SupervisorRunning,
		signalSupervisor:  service.SignalSupervisorStop,
		removeLegacy:      service.RemoveLegacyService,
		setAutostart:      service.SetAutostart,
		call: func(ctx context.Context, op string) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, nil)
		},
		startLauncher: startLauncher,
		sleep:         time.Sleep,
	}
}

func (d windowsUpDeps) waitPolls() int {
	if d.polls > 0 {
		return d.polls
	}
	return 120
}

// needsAdminError ends the command with exit status 21 (needs_admin) after
// printing msg.
type needsAdminError struct{ msg string }

func (e *needsAdminError) Error() string { return e.msg }

// windowsPreflight runs before anything is saved or changed: it removes an
// older LocalSystem service when this process may (an administrator), refuses
// when it may not, and refuses an elevated prompt, whose token the Worker
// would inherit. done is true when the command has nothing more to do.
func windowsPreflight(ctx context.Context, d windowsUpDeps, out io.Writer) error {
	if d.isSystem != nil && d.isSystem() {
		return service.ErrRunsAsSystem
	}
	elev := d.elevation()
	st, qerr := d.query()
	if qerr != nil {
		// Fail closed: the state of the old service is unknown, and it may
		// still be registered against a file the user can replace.
		return &needsAdminError{msg: service.LegacyQueryFailure(qerr)}
	}
	if service.IsLegacyService(st) {
		if !elev.Admin {
			return &needsAdminError{msg: "the Windows service " + service.WindowsServiceName +
				" of an older release runs minion as LocalSystem and must be removed before the Worker runs as you; " + service.LegacyRemovalCommand +
				", then run minion worker up again here"}
		}
		_, _ = fmt.Fprintf(out, "Removing the LocalSystem service %s of an older release (the Worker now runs as you).\n", service.WindowsServiceName)
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := d.removeLegacy(rctx)
		cancel()
		switch {
		case errors.Is(err, service.ErrLegacyRemovalPending):
			_, _ = fmt.Fprintf(out, "The service %s is stopped and marked for deletion; Windows removes it when the Services console is closed.\n", service.WindowsServiceName)
		case err != nil:
			return err
		default:
			// Confirm: the service must be gone before anything is installed.
			if st2, qerr := d.query(); qerr != nil {
				return &needsAdminError{msg: service.LegacyQueryFailure(qerr)}
			} else if st2.Installed && st2.Running {
				return errors.New("the service " + service.WindowsServiceName + " still runs after its removal; " + service.LegacyRemovalCommand)
			}
			_, _ = fmt.Fprintf(out, "Removed the service %s.\n", service.WindowsServiceName)
		}
	}
	if elev.Elevated {
		return errors.New("this terminal runs as administrator (elevated); the Worker runs with the rights of the prompt that starts it, " +
			"so run minion worker up again in a normal terminal, not \"as administrator\"")
	}
	return nil
}

// installWindowsAutostart writes the Run entry, replaces a running Worker of
// this user (so that the new settings apply) and starts the supervisor now.
func installWindowsAutostart(ctx context.Context, d windowsUpDeps, binary, stateDir string, pinState bool, out io.Writer) error {
	var args []string
	if pinState {
		args = []string{"--state-dir", stateDir}
	}
	runCmd, err := service.AutostartCommand(binary, args)
	if err != nil {
		return err
	}
	// Replace what runs first, and only then write the new Run value: a
	// replacement that fails leaves the old entry as it was. A supervisor is
	// signaled directly (it may be in its back-off or between a Worker's
	// exit and restart, with no pipe to ask); the pipe stops a foreground
	// Worker that has no supervisor.
	if d.signalSupervisor != nil {
		if _, err := d.signalSupervisor(); err != nil {
			return fmt.Errorf("cannot stop the running Worker supervisor: %w", err)
		}
	}
	if d.live(ctx) {
		_, _ = fmt.Fprintln(out, "Stopping the running Worker so that it starts again with these settings.")
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := d.call(cctx, agent.OpLocalStop)
		cancel()
		if err != nil && !errors.Is(err, client.ErrWorkerNotRunning) {
			return fmt.Errorf("cannot ask the running Worker to stop: %w", err)
		}
		if !d.waitLive(ctx, false) {
			return errors.New("the running Worker did not stop within 30 seconds; stop it (minion worker down) and run the command again")
		}
	}
	// The pipe closes before the old Worker and its supervisor are gone; a
	// new supervisor that starts meanwhile finds the mutex taken and ends,
	// leaving no Worker. Wait for the old supervisor to leave.
	if !d.waitSupervisorGone() {
		return errors.New("the previous Worker supervisor did not end within 30 seconds; stop it (minion worker down) and run the command again")
	}
	if err := d.setAutostart(runCmd); err != nil {
		return fmt.Errorf("cannot write the Run entry %s of HKCU\\%s: %w", service.AutostartValueName, service.AutostartRunKey, err)
	}
	if err := d.startLauncher(binary, args); err != nil {
		return fmt.Errorf("cannot start the Worker: %w", err)
	}
	if !d.waitLive(ctx, true) {
		return errors.New("the Worker did not start within 30 seconds; its log is " + launchLogName + " in the state directory " + stateDir)
	}
	// The Worker that answers must be the replacement, serving the requested
	// state directory (not another Worker of this user).
	if got := d.liveStateDir(ctx); got != "" && !sameWinDir(got, stateDir) {
		return fmt.Errorf("a Worker answers, but it serves the state directory %s, not %s; stop it (minion worker down) and run the command again", got, stateDir)
	}
	_, _ = fmt.Fprintf(out, "Installed the Worker for this user: it runs as you, starts at each logon (HKCU\\%s, value %s) and starts again by itself after a failure or an update. Stop it with: minion worker down\n",
		service.AutostartRunKey, service.AutostartValueName)
	return nil
}

// live reports whether a Worker of this user answers on its pipe.
func (d windowsUpDeps) live(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := d.call(cctx, agent.OpLocalStatus)
	return err == nil
}

// waitLive waits until the Worker answers (want) or no longer does.
func (d windowsUpDeps) waitLive(ctx context.Context, want bool) bool {
	for i := 0; i < d.waitPolls(); i++ {
		if d.live(ctx) == want {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		d.sleep(250 * time.Millisecond)
	}
	return false
}

// waitSupervisorGone waits until no supervisor of this session holds its
// mutex.
func (d windowsUpDeps) waitSupervisorGone() bool {
	if d.supervisorRunning == nil {
		return true
	}
	for i := 0; i < d.waitPolls(); i++ {
		if !d.supervisorRunning() {
			return true
		}
		d.sleep(250 * time.Millisecond)
	}
	return false
}

// liveStateDir returns the state directory the answering Worker names, or ""
// when it does not say.
func (d windowsUpDeps) liveStateDir(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := d.call(cctx, agent.OpLocalStatus)
	if err != nil {
		return ""
	}
	var reply agent.LiveStatusReply
	if json.Unmarshal(raw, &reply) != nil {
		return ""
	}
	return reply.StateDir
}

// sameWinDir compares two Windows paths: cleaned, case-insensitive.
func sameWinDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
