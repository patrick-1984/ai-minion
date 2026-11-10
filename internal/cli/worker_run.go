package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
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
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerRun(cmd, deps)
		},
	}
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}

// runWorkerRun resolves the settings, starts the Worker runtime and runs it
// until the context ends or a signal arrives.
func runWorkerRun(cmd *cobra.Command, deps workerRunDeps) error {
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
	}

	level, err := res.Resolve(config.KeyLogLevel)
	if err != nil {
		return err
	}
	format, err := res.Resolve(config.KeyLogFormat)
	if err != nil {
		return err
	}
	stderr := deps.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	logger, err := logx.New(logx.Options{Level: level.Value, Format: logx.Format(format.Value), Output: stderr})
	if err != nil {
		return err
	}

	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return err
	}
	reg := deps.registry
	if reg == nil {
		reg = agent.DefaultRegistry
	}
	rt := agent.NewRuntime(reg, deps.options)
	err = rt.Run(cmd.Context(), agent.Settings{
		WorkerCore: core,
		Resolver:   res,
		Logger:     logger,
		Out:        cmd.OutOrStdout(),

		InviteArgvWarned: deps.inviteArgvWarned,
	})
	if err != nil && !onlyCanceled(err) {
		return err
	}
	return nil
}

// onlyCanceled reports whether err is nothing but a clean cancellation (a
// signal that arrived while the Worker was starting). A joined error that also
// carries an independent failure, such as a module that failed to stop during
// the rollback, is not clean and must be reported.
func onlyCanceled(err error) bool {
	switch e := err.(type) {
	case nil:
		return true
	case interface{ Unwrap() []error }:
		for _, c := range e.Unwrap() {
			if !onlyCanceled(c) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if next := e.Unwrap(); next != nil {
			return onlyCanceled(next)
		}
		return false
	}
	return err == context.Canceled
}

// addSettingFlags adds one flag for every setting that role resolves and that
// has a flag, so that a setting declared by any story appears on the command
// without editing it (AD-19). A flag is only a source when the user sets it
// (see changedFlags), so its empty default never wins over the environment.
// A setting whose only choices are true and false is a switch: "--preview-only" alone
// means true.
func addSettingFlags(cmd *cobra.Command, reg *config.Registry, role config.Scope) {
	for _, s := range reg.ForScope(role) {
		if !s.HasFlag() || cmd.Flags().Lookup(s.FlagName()) != nil {
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
	if len(c) != 2 {
		return false
	}
	if c[0] > c[1] {
		c[0], c[1] = c[1], c[0]
	}
	return reflect.DeepEqual(c, []string{"false", "true"}) && strings.EqualFold(s.Default, "false")
}
