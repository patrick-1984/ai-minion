package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerStartCmd(defaultWorkerStartDeps()))
	})
}

// workerStartDeps are the inputs of "minion worker start" that tests replace.
type workerStartDeps struct {
	goos      string
	targetEnv service.TargetEnv
	service   service.Deps
	// windows returns the controller of the Windows service.
	windows func() (service.Controller, error)
}

func defaultWorkerStartDeps() workerStartDeps {
	return workerStartDeps{
		goos:      runtime.GOOS,
		targetEnv: service.SystemTargetEnv(),
		service:   withEnv(service.DefaultDeps()),
		windows: func() (service.Controller, error) {
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			return service.NewKardianosController(exe)
		},
	}
}

// workerStartResult is the JSON result of "worker start".
type workerStartResult struct {
	service.StartResult
	StartHint string `json:"start_hint,omitempty"`
}

func newWorkerStartCmd(deps workerStartDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the Worker service that worker up installed (never installs)",
		Long: `Start the Worker service, unit or agent that "minion worker up" installed: the
systemd user unit or system service minion-worker@<user>.service, the launchd
agent, or the Windows service MinionWorker. It never installs or changes
anything; without an installed service it exits with status 26 (set it up with
minion worker up). When starting needs rights this process does not have (a
system service without sudo, the Windows service without an elevated prompt) it
exits with status 21 and names the command that has them.

--state-dir D starts the service only when it is registered for the Worker of
that state directory (the one it pins, else the default one); another
profile's service is left stopped and the command exits 27 (the tray passes
its profile this way).

--json prints one JSON object {"ok","code","message","manager","scope","unit",...}.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, derr := explicitStateDir(cmd)
			if derr != nil {
				return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, workerStartResult{}, derr)
			}
			res, err := runWorkerStart(cmd, deps, dir)
			if err == nil && !asJSON {
				switch {
				case res.AlreadyRunning:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The Worker (%s) is already running.\n", res.Unit)
				default:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Started the Worker (%s).\n", res.Unit)
				}
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, res, err)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

func runWorkerStart(cmd *cobra.Command, deps workerStartDeps, wantDir string) (workerStartResult, error) {
	var res workerStartResult
	req := service.StartRequest{GOOS: deps.goos, EUID: deps.targetEnv.EUID, Windows: deps.windows}
	if deps.goos != "windows" {
		t, err := service.ResolveTarget(deps.targetEnv, "")
		if err != nil {
			return res, err
		}
		req.User, req.Home, req.UID = t.User, t.Home, t.UID
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if wantDir != "" && deps.goos != "windows" {
		if got, ok := unitStateDir(ctx, deps.service, deps.goos, req.User, req.Home); ok && !sameDir(got, wantDir) {
			return res, &resultError{Exit: ExitStateChanged, Message: fmt.Sprintf(
				"the installed service is registered for the state directory %s, not %s; it was not started and nothing was changed", got, wantDir),
				Fields: map[string]any{"service_state_dir": got}}
		}
	}
	sr, err := deps.service.StartInstalled(ctx, req)
	res.StartResult = sr
	var na *service.NeedsAdminError
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		return res, &resultError{Exit: ExitNotInstalled, Message: err.Error(), Err: err}
	case errors.As(err, &na):
		res.StartHint = na.Hint
		return res, &resultError{Exit: ExitNeedsAdmin, Message: err.Error(), Err: err, Fields: map[string]any{"start_hint": na.Hint}}
	}
	return res, err
}

// unitStateDir returns the state directory the installed service of the
// account runs: the MINION_STATE_DIR it pins, else the default one. ok is
// false when no service is installed or it cannot be read (the start's own
// refusals then apply).
func unitStateDir(ctx context.Context, d service.Deps, goos, user, home string) (string, bool) {
	if d.Runner == nil {
		return "", false
	}
	var env string
	switch goos {
	case "linux":
		p := d.ProbeSystemInstance(ctx, user, home)
		if p.Manager != service.ManagerSystemd {
			return "", false
		}
		args := []string{"show", "-p", "Environment", "--value", p.Unit}
		if p.Scope == service.ScopeUser {
			args = append([]string{"--user"}, args...)
		}
		out, err := d.Runner.Run(ctx, "systemctl", args...)
		if err != nil {
			return "", false
		}
		env = out
	case "darwin":
		b, err := os.ReadFile(service.PlistPath(home))
		if err != nil {
			return "", false
		}
		env = string(b)
		if i := strings.Index(env, "MINION_STATE_DIR</key>"); i >= 0 {
			rest := env[i+len("MINION_STATE_DIR</key>"):]
			if a := strings.Index(rest, "<string>"); a >= 0 {
				if e := strings.Index(rest[a:], "</string>"); e > 0 {
					return strings.TrimSpace(rest[a+len("<string>") : a+e]), true
				}
			}
		}
		env = ""
	default:
		return "", false
	}
	for _, kv := range strings.Fields(env) {
		if v, ok := strings.CutPrefix(kv, "MINION_STATE_DIR="); ok {
			return strings.Trim(v, `"`), true
		}
	}
	if def := defaultStateDirOf(); def != "" {
		return def, true
	}
	return "", false
}
