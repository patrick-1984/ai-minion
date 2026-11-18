package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

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

--json prints one JSON object {"ok","code","message","manager","scope","unit",...}.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runWorkerStart(cmd, deps)
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
	return cmd
}

func runWorkerStart(cmd *cobra.Command, deps workerStartDeps) (workerStartResult, error) {
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
