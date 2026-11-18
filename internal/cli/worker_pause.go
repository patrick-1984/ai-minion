package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

func init() {
	Register(func(root *cobra.Command) {
		w := workerCommand(root)
		w.AddCommand(newWorkerPauseCmd(defaultWorkerPauseDeps(), true))
		w.AddCommand(newWorkerPauseCmd(defaultWorkerPauseDeps(), false))
	})
}

// workerPauseDeps are the inputs of "minion worker pause|resume" that tests
// replace.
type workerPauseDeps struct {
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// lockState takes the lifecycle lock of the state directory without
	// waiting (held by every running Worker); agent.ErrStateInUse means a
	// Worker runs.
	lockState func(stateDir string) (io.Closer, error)
	// lockConfig takes the config lock (.config.lock) of the state directory.
	lockConfig func(path string) (func(), error)
	// setPause writes the pause file (agent.SetPause).
	setPause func(stateDir string, paused bool, until, now time.Time) (agent.PauseState, error)
	// call is the local control socket of this user.
	call func(ctx context.Context, op string, args any) (json.RawMessage, error)
	now  func() time.Time
}

func defaultWorkerPauseDeps() workerPauseDeps {
	return workerPauseDeps{
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		lockState:  agent.LockLifecycle,
		lockConfig: lockConfigFile,
		setPause:   agent.SetPause,
		call: func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		},
		now: time.Now,
	}
}

// pauseResult is the JSON result of pause and resume (besides ok, code and
// message).
type pauseResult struct {
	Paused     bool   `json:"paused"`
	Until      string `json:"until,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	// Applied is "offline" (no Worker runs; it applies the file at its
	// start) or "live" (the running Worker applied and acknowledged it).
	Applied string `json:"applied,omitempty"`
}

func newWorkerPauseCmd(deps workerPauseDeps, pause bool) *cobra.Command {
	var asJSON bool
	var dur time.Duration
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume the Hub traffic of this Worker after a pause",
		Long: `Resume the Hub traffic of this Worker: it connects to its Hub again.

Without a running Worker the saved pause is cleared and the Worker connects at
its next start. A running Worker is asked to resume over the local control
socket; a Worker that cannot pause (an older version) refuses with exit status
20 and nothing is changed.

--json prints one JSON object {"ok","code","message","paused",...}.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerPause(cmd, deps, false, 0, asJSON)
		},
	}
	if pause {
		cmd.Use = "pause"
		cmd.Short = "Pause all Hub traffic of this Worker (local sessions keep running)"
		cmd.Long = `Pause all automatic Hub traffic of this Worker: it disconnects from its Hub and
does not connect, pair or enroll until "minion worker resume" or the end of
--for. Sessions keep running and stay usable on this machine; nothing ends.

Without a running Worker the pause is saved in the state directory
(pause.json) and applies at the next start. A running Worker is asked to pause
over the local control socket; a Worker that cannot pause (an older version)
refuses with exit status 20 and nothing is saved. A Worker without a Hub
(Solo) has nothing to pause.

--json prints one JSON object {"ok","code","message","paused","until",...}.`
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("for") && dur <= 0 {
				return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, nil, &usageError{errors.New("--for needs a positive duration, for example 30m or 2h")})
			}
			return runWorkerPause(cmd, deps, true, dur, asJSON)
		}
		cmd.Flags().DurationVar(&dur, "for", 0, "end the pause after this long (for example 30m); without it the pause lasts until resumed")
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

func runWorkerPause(cmd *cobra.Command, deps workerPauseDeps, pause bool, dur time.Duration, asJSON bool) error {
	out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	res, err := doWorkerPause(cmd, deps, pause, dur)
	if err == nil && !asJSON {
		switch {
		case !res.Paused:
			_, _ = fmt.Fprintln(out, "Resumed: the Worker connects to its Hub again.")
		case res.Until != "":
			_, _ = fmt.Fprintf(out, "Paused: no Hub traffic until %s (or minion worker resume).\n", res.Until)
		default:
			_, _ = fmt.Fprintln(out, "Paused: no Hub traffic until minion worker resume.")
		}
		if res.Applied == "offline" {
			_, _ = fmt.Fprintln(out, "No Worker is running on this state directory; it applies this at its next start.")
		}
	}
	return emitResult(out, stderr, asJSON, res, err)
}

func doWorkerPause(cmd *cobra.Command, deps workerPauseDeps, pause bool, dur time.Duration) (pauseResult, error) {
	var res pauseResult
	r := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(r, deps.loadConfig)
		if err != nil {
			return res, err
		}
		r.File = file
	}
	dir, err := r.Resolve(config.KeyStateDir)
	if err != nil {
		return res, err
	}
	stateDir, err := filepath.Abs(dir.Value)
	if err != nil {
		return res, err
	}
	if pause {
		server, err := r.Resolve(config.KeyServer)
		if err != nil {
			return res, err
		}
		if server.Value == "" {
			return res, newResultError(ExitError, "no Hub configured: nothing to pause (this Worker runs Solo)")
		}
	}
	now := deps.now()
	var until time.Time
	if pause && dur > 0 {
		until = now.Add(dur)
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	l, err := deps.lockState(stateDir)
	switch {
	case err == nil:
		// No Worker runs: the CLI owns the state and writes the file.
		defer func() { _ = l.Close() }()
		release, err := deps.lockConfig(filepath.Join(stateDir, configLockName))
		if err != nil {
			return res, err
		}
		defer release()
		p, err := deps.setPause(stateDir, pause, until, now)
		if err != nil {
			return res, fmt.Errorf("the pause was not saved: %w", err)
		}
		return pauseResultOf(p, "offline"), nil
	case !errors.Is(err, agent.ErrStateInUse):
		return res, fmt.Errorf("cannot take the lifecycle lock of the state directory %s (%w); nothing was changed", stateDir, err)
	}

	// A Worker runs: it owns the state and applies the change itself.
	reply, ok := livePauseStatus(ctx, deps, stateDir)
	if !ok {
		return res, newResultError(ExitError, fmt.Sprintf("a Worker is running on the state directory %s and does not answer on this user's control socket; nothing was changed", stateDir))
	}
	op := agent.OpLocalResume
	if pause {
		op = agent.OpLocalPause
	}
	args := agent.LocalPauseArgs{Instance: reply.Instance}
	if !until.IsZero() {
		args.Until = until.UTC().Format(time.RFC3339)
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	raw, err := deps.call(cctx, localapi.QualifiedOp(op, []string{agent.FeaturePause}), args)
	if err != nil {
		return res, err
	}
	var p agent.PauseState
	if err := json.Unmarshal(raw, &p); err != nil {
		return res, fmt.Errorf("the running Worker returned an invalid pause result: %w", err)
	}
	return pauseResultOf(p, "live"), nil
}

func pauseResultOf(p agent.PauseState, applied string) pauseResult {
	r := pauseResult{Paused: p.Paused, Generation: p.Generation, Applied: applied}
	if p.Paused && !p.Until.IsZero() {
		r.Until = p.Until.UTC().Format(time.RFC3339)
	}
	return r
}

// livePauseStatus asks the running Worker of this user for its status; ok is
// false when none answers for stateDir.
func livePauseStatus(ctx context.Context, deps workerPauseDeps, stateDir string) (agent.LiveStatusReply, bool) {
	var reply agent.LiveStatusReply
	if deps.call == nil {
		return reply, false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := deps.call(cctx, agent.OpLocalStatus, nil)
	if err != nil || json.Unmarshal(raw, &reply) != nil || reply.StateDir == "" || !sameDir(reply.StateDir, stateDir) {
		return agent.LiveStatusReply{}, false
	}
	return reply, true
}
