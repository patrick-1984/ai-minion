package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func init() {
	Register(func(root *cobra.Command) {
		worker := workerCommand(root)
		worker.AddCommand(newWorkerRunCmd(defaultWorkerRunDeps(os.Stderr)))
	})
}

// workerCommand returns the "worker" command group, creating it when no other
// file has registered it yet. Several stories add subcommands to it.
func workerCommand(root *cobra.Command) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == "worker" {
			return c
		}
	}
	worker := &cobra.Command{
		Use:   "worker",
		Short: "Run and manage the Minion Worker",
	}
	root.AddCommand(worker)
	return worker
}

// workerRunDeps are the inputs of "minion worker run" that tests replace.
type workerRunDeps struct {
	stderr     io.Writer
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// registry holds the modules and pre-start hooks; nil means
	// agent.DefaultRegistry.
	registry *agent.Registry
	options  agent.Options
	// inviteArgvWarned is set by "worker up" when it already printed the
	// process-list warning of a literal --invite-key, so that the enrollment
	// hook does not print a second one.
	inviteArgvWarned bool
}

func defaultWorkerRunDeps(stderr io.Writer) workerRunDeps {
	return workerRunDeps{stderr: stderr, lookupEnv: os.LookupEnv, loadConfig: loadWorkerConfig}
}

func newWorkerRunCmd(deps workerRunDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the Minion Worker in the foreground",
		Long: `Run the Minion Worker in the foreground until SIGINT or SIGTERM.

The Worker opens its state database, resolves the tmux socket once (from
--tmux-socket, else the user's default tmux socket, as an absolute path),
runs the pre-start hooks, starts its modules in order and serves the sessions
of that tmux server. It needs no Hub. On a signal it stops the modules in
reverse order within 10 seconds.

Every setting has a flag, an environment variable (MINION_ plus the upper-case
name, for example MINION_TMUX_SOCKET) and a key in the config file in the
state directory; a flag wins over the environment, which wins over the file.
Sessions are shared only when you say so: --share-existing all|none|later.`,
		Args: noArgsRedacted,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerRun(cmd, deps)
		},
	}
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	addJoinFlag(cmd, config.Default)
	return cmd
}

// runWorkerRun resolves the settings, starts the Worker runtime and runs it
// until the context ends or a signal arrives.
func runWorkerRun(cmd *cobra.Command, deps workerRunDeps) error {
	lookupEnv := deps.lookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	join, err := expandJoinToken(cmd, config.Default, lookupEnv)
	if err != nil {
		return err
	}
	if join.Applied {
		if join.Warning != "" {
			w := deps.stderr
			if w == nil {
				w = cmd.ErrOrStderr()
			}
			_, _ = fmt.Fprintln(w, join.Warning)
		}
		// The invitation key came from the token: the enrollment hook must
		// not warn about a literal --invite-key that the user never typed.
		deps.inviteArgvWarned = true
	}
	res := &config.Resolver{
		Role:      config.ScopeWorker,
		Flags:     changedFlags{cmd: cmd, reg: config.Default},
		LookupEnv: deps.lookupEnv,
	}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(res, deps.loadConfig)
		if err != nil {
			return err
		}
		res.File = file
		if err := joinFileConflict(cmd, config.Default, file); err != nil {
			return err
		}
	}

	stderr := deps.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	logger, live, err := workerLogger(cmd, res, stderr)
	if err != nil {
		return err
	}

	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return err
	}
	sinks, startSinks := newWorkerSinks(res, live, logger, core.StateDir)
	// Stopped by the log sinks module, last in the stop sequence; this is
	// the fallback when the Worker never got that far.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), agent.LogSinksStopLimit)
		defer cancel()
		_ = sinks.Shutdown(ctx)
	}()
	reg := deps.registry
	if reg == nil {
		reg = agent.DefaultRegistry
	}
	rt := agent.NewRuntime(reg, deps.options)
	err = rt.Run(cmd.Context(), agent.Settings{
		WorkerCore: core,
		Resolver:   res,
		Logger:     logger,
		LogLive:    live,

		LogSinks:      sinks.Status,
		StartLogSinks: startSinks,
		StopLogSinks:  sinks.Shutdown,
		Out:          cmd.OutOrStdout(),

		InviteArgvWarned: deps.inviteArgvWarned,
	})
	if err != nil && !onlyCanceled(err) {
		if exe := agent.RestartPending(); exe != "" {
			// The stop before the restart did not finish cleanly: let this
			// process's exit release everything, and start the new binary so
			// that it waits for that exit.
			logger.Error("the Worker did not stop cleanly before its restart; restarting anyway", "error", err.Error())
			agent.RestartDetached(logger, exe)
		}
		return err
	}
	if exe := agent.RestartPending(); exe != "" {
		// A foreground self-update on Windows: the new binary runs in this
		// console, and this process waits for it.
		logger.Debug("the Worker stopped for its restart; running the new binary in this console", "exe", exe)
		return restartInConsole(exe)
	}
	return nil
}

// onlyCanceled reports whether err is nothing but a clean cancellation
// (agent.OnlyCanceled).
func onlyCanceled(err error) bool { return agent.OnlyCanceled(err) }

// addSettingFlags adds one flag for every setting that role resolves and that
// has a flag, so that a setting declared by any story appears on the command
// without editing it (AD-19). A flag is only a source when the user sets it
// (see changedFlags), so its empty default never wins over the environment.
// A setting whose only choices are true and false is a switch: "--preview-only" alone
// means true.
func addSettingFlags(cmd *cobra.Command, reg *config.Registry, role config.Scope) {
	for _, s := range reg.ForScope(role) {
		// --join is added only to the commands that expand it (addJoinFlag).
		if !s.HasFlag() || s.Key == config.KeyVerbose || s.Key == config.KeyJoin || cmd.Flags().Lookup(s.FlagName()) != nil {
			continue
		}
		help := s.Help
		if help == "" {
			help = s.Key
		}
		help = fmt.Sprintf("%s (env %s)", help, s.EnvName())
		cmd.Flags().String(s.FlagName(), "", help)
		if isSwitch(s) {
			cmd.Flags().Lookup(s.FlagName()).NoOptDefVal = "true"
		}
	}
}

func isSwitch(s config.Setting) bool {
	c := append([]string(nil), s.Choices...)
	slices.Sort(c)
	return (reflect.DeepEqual(c, []string{"false", "true"}) || reflect.DeepEqual(c, []string{"0", "1", "false", "true"})) &&
		strings.EqualFold(s.Default, "false")
}
