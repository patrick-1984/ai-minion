package cli

// minion worker pause / resume: one persisted local choice, until resumed.
// A paused Worker has closed its Hub link and makes no Hub request (no
// reconnect, no automatic pairing). Local control and the terminal sessions
// that are open keep working, and a restart keeps the pause.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func init() {
	Register(func(root *cobra.Command) {
		w := workerCommand(root)
		d := defaultWorkerPauseDeps()
		w.AddCommand(newWorkerPauseCmd(d, true), newWorkerPauseCmd(d, false))
	})
}

// workerPauseDeps are the seams of pause and resume that tests replace.
type workerPauseDeps struct {
	lookupEnv func(string) (string, bool)
	// call is the local control socket of this user.
	call func(ctx context.Context, op string, args any) (json.RawMessage, error)
}

func defaultWorkerPauseDeps() workerPauseDeps {
	return workerPauseDeps{
		lookupEnv: os.LookupEnv,
		call: func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		},
	}
}

func newWorkerPauseCmd(d workerPauseDeps, pause bool) *cobra.Command {
	use, short := "resume", "Let a paused Worker connect to its Hub again"
	long := "Resume a paused Worker: it connects to its Hub again and pairs by itself when it has to."
	if pause {
		use, short = "pause", "Close the Hub link and stay off the Hub until resumed"
		long = "Pause the Worker until you resume it. The Hub link is closed and the Worker makes no Hub request (no\n" +
			"reconnect, no automatic pairing) before this command reports success. Local control (this CLI, mn, MCP)\n" +
			"and the terminal sessions that are open keep working. The pause is saved: a restart keeps it, and\n" +
			"\"minion worker status\" shows it. Undo it with: minion worker resume"
	}
	cmd := &cobra.Command{
		Use: use, Short: short, Long: long, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runWorkerPause(cmd, d, pause) },
	}
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

func runWorkerPause(cmd *cobra.Command, d workerPauseDeps, pause bool) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	res := &config.Resolver{Role: config.ScopeWorker, Registry: config.Default, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: d.lookupEnv}
	dir, err := res.Resolve(config.KeyStateDir)
	if err != nil {
		return err
	}
	stateDir, err := filepath.Abs(dir.Value)
	if err != nil || dir.Value == "" {
		return errors.New("the state directory is not known; give --state-dir or set MINION_STATE_DIR")
	}
	out := cmd.OutOrStdout()

	cctx, cancel := context.WithTimeout(ctx, agent.PauseSettleTimeout+10*time.Second)
	defer cancel()
	live, err := d.liveFor(cctx, stateDir, dir.Source != config.SourceDefault)
	if err != nil {
		return err
	}
	if live {
		_, err := d.call(cctx, agent.OpLocalPause, map[string]bool{"paused": pause})
		var pe *proto.Error
		switch {
		case err == nil:
		case errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp:
			return errors.New("the running Worker is older than this command and cannot pause; restart it (minion worker down, then minion worker start) after updating minion")
		default:
			return fmt.Errorf("the Worker did not confirm: %w", err)
		}
	} else {
		// Only a running Worker pauses: it is the one that saves the choice in
		// the profile it serves, so the pause cannot land in a state directory
		// that the Worker which starts later does not read.
		return errors.New("no Worker is running for this user; start it with: minion worker start, then run this command again")
	}
	if pause {
		return writef(out, "The Worker is paused: its Hub link is closed and it makes no Hub request. Local control and open terminal sessions keep working. Resume with: minion worker resume\n")
	}
	return writef(out, "The Worker is resumed: it connects to its Hub again.\n")
}

// liveFor reports whether a Worker of this state directory answers. A Worker
// of another state directory is not the one this command is about.
//
// Without an explicit state directory the running Worker's own profile is the
// one that is meant, whatever its directory.
func (d workerPauseDeps) liveFor(ctx context.Context, stateDir string, explicit bool) (bool, error) {
	raw, err := d.call(ctx, agent.OpLocalStatus, nil)
	if errors.Is(err, client.ErrWorkerNotRunning) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot ask the running Worker: %w", err)
	}
	var reply agent.LiveStatusReply
	if json.Unmarshal(raw, &reply) == nil && explicit && reply.StateDir != "" && !sameDir(reply.StateDir, stateDir) {
		return false, nil
	}
	return true, nil
}
