package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerRestartCmd(defaultWorkerRestartDeps()))
	})
}

// workerRestartDeps are the inputs of "minion worker restart".
type workerRestartDeps struct {
	call  func(ctx context.Context, op string, args any) (json.RawMessage, error)
	sleep func(time.Duration)
	// wait bounds the wait for the new instance.
	wait time.Duration
}

func defaultWorkerRestartDeps() workerRestartDeps {
	return workerRestartDeps{
		call: func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		},
		sleep: time.Sleep,
		wait:  60 * time.Second,
	}
}

// workerRestartResult is the JSON result of "worker restart".
type workerRestartResult struct {
	StateDir     string `json:"state_dir,omitempty"`
	OldInstance  string `json:"old_instance,omitempty"`
	Instance     string `json:"instance,omitempty"`
	SessionsLost *int   `json:"sessions_lost,omitempty"`
}

func newWorkerRestartCmd(d workerRestartDeps) *cobra.Command {
	var acceptLoss int
	var acceptUnknown, asJSON bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the running Worker in place (same state directory and run mode)",
		Long: `Restart the running Worker in place: the same binary, state directory and run
mode (a service exits with the restart code and its manager starts it again, a
foreground Worker replaces itself). Nothing is installed or reconfigured.

Sessions that live inside the Worker (Windows ConPTY) end: the Worker counts
them first and the command exits 23 with sessions_lost N unless --accept-loss N
is given (29 when they cannot be counted; --accept-unknown-loss accepts that).
The command waits until a new Worker instance answers for the same state
directory. Without a running Worker it exits 24. --json prints one JSON object.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runWorkerRestart(cmd, d, acceptLoss, acceptUnknown)
			if err == nil && !asJSON {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The Worker restarted (state directory %s).\n", res.StateDir)
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, res, err)
		},
	}
	cmd.Flags().IntVar(&acceptLoss, "accept-loss", 0, "accept that up to this many sessions end (ConPTY)")
	cmd.Flags().BoolVar(&acceptUnknown, "accept-unknown-loss", false, "accept that an unknown number of sessions end")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	return cmd
}

func liveReply(ctx context.Context, call func(context.Context, string, any) (json.RawMessage, error)) (agent.LiveStatusReply, error) {
	var r agent.LiveStatusReply
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := call(cctx, agent.OpLocalStatus, nil)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	return r, nil
}

func runWorkerRestart(cmd *cobra.Command, d workerRestartDeps, acceptLoss int, acceptUnknown bool) (workerRestartResult, error) {
	var res workerRestartResult
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	before, err := liveReply(ctx, d.call)
	if errors.Is(err, client.ErrWorkerNotRunning) {
		return res, newResultError(ExitNotRunning, "no Worker is running for this user; start it with minion worker start")
	}
	if err != nil {
		return res, fmt.Errorf("cannot ask the running Worker: %w", err)
	}
	res.StateDir, res.OldInstance = before.StateDir, before.Instance
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	raw, err := d.call(cctx, localapi.QualifiedOp(agent.OpLocalLifecycle, []string{agent.FeatureLifecycle}), agent.LocalLifecycleArgs{
		Action: agent.LifecycleRestart, Instance: before.Instance, AcceptLoss: acceptLoss, AcceptUnknown: acceptUnknown,
	})
	cancel()
	if err != nil {
		if rerr := lifecycleRefusal(err); rerr != nil {
			return res, rerr
		}
		var pe *proto.Error
		if errors.Is(err, client.ErrWorkerTooOld) || (errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp) {
			return res, &resultError{Exit: ExitWorkerTooOld, Message: "the running Worker is too old to restart itself; stop it and start it again (minion worker stop, minion worker start)", Err: err}
		}
		return res, fmt.Errorf("cannot ask the running Worker to restart: %w", err)
	}
	var lr agent.LifecycleResult
	if json.Unmarshal(raw, &lr) == nil && lr.SessionsLost >= 0 {
		n := lr.SessionsLost
		res.SessionsLost = &n
	}
	// Restarted means: a new instance answers for the same state directory.
	deadline := time.Now().Add(d.wait)
	for time.Now().Before(deadline) {
		d.sleep(250 * time.Millisecond)
		r, err := liveReply(ctx, d.call)
		if err == nil && r.Instance != "" && r.Instance != before.Instance && sameDir(r.StateDir, before.StateDir) {
			res.Instance = r.Instance
			return res, nil
		}
	}
	return res, &resultError{Exit: ExitError, Message: "the Worker was asked to restart, but no new instance answered in time; check it with minion worker status"}
}
