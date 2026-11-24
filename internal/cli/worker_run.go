package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"

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
	// migrateLogin updates the shell login blocks of earlier releases
	// (migrateLoginStartup) once the Worker started; nil skips it. It
	// publishes a change only through gate.
	migrateLogin func(ctx context.Context, stateDir string, gate *loginGate) (loginMigration, error)
}

// loginMigration is what the update of the shell login blocks of earlier
// releases did.
type loginMigration struct {
	// Changed are the startup files it updated, Backups the copy of each
	// (same index) that it wrote before.
	Changed, Backups []string
	// Skipped says why a file with an earlier block was left as it is, or
	// what else the user should know about an update.
	Skipped []string
	// Pending are the startup files (as named in the home directory) that
	// still hold a block of an earlier release the update could not replace
	// safely; Skipped tells the user how to repair each.
	Pending []string
}

// errLoginRevoked is the result of a login block update that was about to
// publish a change after the Worker began to stop.
var errLoginRevoked = errors.New("the Worker is stopping: the login block update was abandoned")

// loginGate ties the publication of a login block update to the lifecycle
// of the Worker that runs it: the Worker revokes it in its stop sequence,
// before it releases the lifecycle lock, and from then on no change is
// published. Revoke waits for a publication in progress (a few local file
// operations), so none can complete after it returned. A nil gate only checks
// the context (the installer).
type loginGate struct {
	mu      sync.Mutex
	revoked bool
}

// revoke ends the gate; it returns once no publication is in progress.
func (g *loginGate) revoke() {
	g.mu.Lock()
	g.revoked = true
	g.mu.Unlock()
}

func defaultWorkerRunDeps(stderr io.Writer) workerRunDeps {
	return workerRunDeps{stderr: stderr, lookupEnv: os.LookupEnv, loadConfig: loadWorkerConfig, migrateLogin: migrateLoginStartup}
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
	if err := service.RefuseSystem(systemAccount()); err != nil {
		return err
	}
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
	// The exporter installer runs inside the locked runtime (the exporter
	// module) and is joined before the lock is released. It re-reads the
	// config file before every pass, so a saved "off" applies at once.
	policy := func() *config.Resolver {
		r := *res
		if deps.loadConfig != nil {
			file, err := loadConfigFile(&r, deps.loadConfig)
			if err != nil {
				return nil // unreadable policy: the installer does nothing
			}
			r.File = file
		}
		return &r
	}
	startExporter := func(ctx context.Context) func() {
		claudeInstallNotice = func(msg string, args ...any) { logger.Warn(msg, args...) }
		autoCtx, stopAuto := context.WithCancel(ctx)
		autoDone := make(chan struct{})
		go func() {
			defer close(autoDone)
			runUsageAutoInstall(autoCtx, policy, core.StateDir, lookupEnv)
		}()
		return func() { stopAuto(); <-autoDone }
	}
	// An updated Worker fixes the shell login blocks earlier releases wrote:
	// only once it started (it holds the state's lifecycle lock and answers
	// on its socket), never in a failed start, and joined before the lock is
	// released. In the background: it waits for the registration lock, which
	// "worker start --at-login" holds until this Worker answers.
	var startLogin func(ctx context.Context) func()
	if deps.migrateLogin != nil {
		startLogin = func(ctx context.Context) func() {
			ctx, cancel := context.WithCancel(ctx)
			gate := &loginGate{}
			done := make(chan struct{})
			go func() {
				defer close(done)
				res, err := deps.migrateLogin(ctx, core.StateDir, gate)
				// At most one line per startup file and one for the error,
				// each bounded in time (logx.BestEffort).
				if err != nil && ctx.Err() == nil && !errors.Is(err, errLoginRevoked) {
					logx.BestEffort(logger, slog.LevelWarn, "could not update the Worker login startup block", "error", err.Error())
				}
				for i, p := range res.Changed {
					logx.BestEffort(logger, slog.LevelInfo, "updated the Worker login startup block", "file", p, "backup", res.Backups[i])
				}
				for _, note := range res.Skipped {
					logx.BestEffort(logger, slog.LevelWarn, "left a Worker login startup block of an earlier release as it is", "reason", note)
				}
			}()
			// Cancel, then revoke before the join: even when the stop
			// sequence gives up waiting for the join, nothing is published
			// once the lifecycle lock is released.
			return func() { cancel(); gate.revoke(); <-done }
		}
	}
	err = rt.Run(cmd.Context(), agent.Settings{
		WorkerCore: core,
		Resolver:   res,
		Logger:     logger,
		LogLive:    live,

		LogSinks:        sinks.Status,
		StartLogSinks:   startSinks,
		StopLogSinks:    sinks.Shutdown,
		TrafficTap:      sinks.Tap(),
		Out:             cmd.OutOrStdout(),
		StartBackground: startExporter,
		StartAfterReady: startLogin,

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

// systemAccount reports whether this process runs as LocalSystem; tests
// replace it.
var systemAccount = service.IsSystemAccount
