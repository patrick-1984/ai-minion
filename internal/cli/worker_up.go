package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

func init() {
	Register(func(root *cobra.Command) {
		w := workerCommand(root)
		w.AddCommand(newWorkerUpCmd(defaultWorkerUpDeps(os.Stderr)))
		w.AddCommand(newWorkerServiceRunCmd(defaultWorkerRunDeps(os.Stderr), service.SCM{}))
	})
}

// workerUpDeps are the inputs of "minion worker up" that tests replace.
type workerUpDeps struct {
	// run supplies the registry, the environment, the config loader and the
	// writers; it is also what the foreground mode runs on.
	run workerRunDeps
	// settings is the registry the flags come from; nil means config.Default.
	settings *config.Registry
	// detectEnv and targetEnv are the facts about the machine.
	detectEnv service.Env
	targetEnv service.TargetEnv
	// executable returns the path of the running binary.
	executable func() (string, error)
	// service holds the ports of the service installation.
	service service.Deps
	// chown gives the files created as root to the target user.
	chown service.Chowner
	// setup holds the seams of the first-run wizard that "worker up" starts
	// for a Worker that was never set up. Fields left empty fall back to the
	// run dependencies, and then to the real thing.
	setup workerSetupDeps
}

// setupDeps returns the wizard dependencies with the environment and config
// loader of the run dependencies filled in.
func (d workerUpDeps) setupDeps() workerSetupDeps {
	s := d.setup
	if s.lookupEnv == nil {
		s.lookupEnv = d.run.lookupEnv
	}
	if s.loadConfig == nil {
		s.loadConfig = d.run.loadConfig
	}
	return s
}

func defaultWorkerUpDeps(stderr io.Writer) workerUpDeps {
	return workerUpDeps{
		run:        defaultWorkerRunDeps(stderr),
		detectEnv:  service.SystemEnv(),
		targetEnv:  service.SystemTargetEnv(),
		executable: os.Executable,
		service:    withEnv(service.DefaultDeps()),
		chown:      service.OSChowner{},
	}
}

func withEnv(d service.Deps) service.Deps {
	d.Getenv = os.Getenv
	return d
}

func (d workerUpDeps) registry() *config.Registry {
	if d.settings != nil {
		return d.settings
	}
	return config.Default
}

func newWorkerUpCmd(deps workerUpDeps) *cobra.Command {
	var foreground bool
	var asUser string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Install the Worker as a service and start it, or run it in the foreground",
		Long: `Provision this machine as a Minion Worker with one command.

On Linux with systemd, run with sudo: the Worker is installed as the system
service minion-worker@<user>.service (User=<user>, Restart=always, ExecStart=
<binary> worker run), enabled at boot and started now. Without sudo a systemd
user service is installed instead; the command then enables lingering for you
(loginctl enable-linger) or, when that is not allowed, prints the exact command
to run and exits with an error. On macOS a launchd agent is installed and
loaded for the current user.

Every Worker setting is a flag (the same flags as "worker run"). The flags,
and the settings given by MINION_* environment variables, are written to the
Worker config file in the state directory so that the service uses them;
secrets are never written. When the state directory is not the default one of
the user (--state-dir, MINION_STATE_DIR, or XDG_STATE_HOME in your shell), it is
written into the service itself (MINION_STATE_DIR), so that the service reads
exactly the directory this command saved to.

A Worker that was never set up (no --share-existing, MINION_SHARE_EXISTING or
config file value, and no choice saved by "minion worker setup") gets the
first-run wizard when this runs in a terminal: what to share, the Hub address
and a pairing code. Without a terminal nothing is asked: the command stops
before it changes anything and names the flags to give. Run as root for
another user it does the same, because root does not ask about, or open, that
user's tmux and state: run "minion worker setup" as that user, or give
--share-existing all|none|later.

"worker up" first runs the pre-start hooks (for example invitation
enrollment) and reports their result. A failing hook stops the command with an
error before any service is installed or changed.

Without a service manager (a container) or with --foreground, the Worker runs
in the foreground exactly like "worker run" with the same settings, until
SIGINT or SIGTERM; the pre-start hooks run as its first step. In a container
configure it with the environment: MINION_SERVER (the Hub URL),
MINION_INVITE_KEY_FILE (path of a file with the invitation key) and
MINION_SHARE_EXISTING (all, none or later; required, since a container has no
terminal to ask on). The Worker identity and its
database live under MINION_STATE_DIR; use /data and mount a volume there so
that the identity survives a restart of the container.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerUp(cmd, deps, foreground, asUser)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Run the Worker in the foreground instead of installing a service")
	cmd.Flags().StringVar(&asUser, "as-user", "", "Install the service for this Unix user (needs root; default: the user that ran sudo)")
	addSettingFlags(cmd, deps.registry(), config.ScopeWorker)
	return cmd
}

func runWorkerUp(cmd *cobra.Command, deps workerUpDeps, foreground bool, asUser string) error {
	out := cmd.OutOrStdout()
	det := service.Detect(deps.detectEnv)
	if foreground || det.Manager == service.ManagerNone {
		if !foreground {
			_, _ = fmt.Fprintf(out, "No service manager (%s): running the Worker in the foreground.\n", det.Reason)
		}
		// The foreground Worker takes the same flags: a secret given on the
		// command line is as visible in the process list as for a service (R85).
		stderr := deps.run.stderr
		if stderr == nil {
			stderr = cmd.ErrOrStderr()
		}
		lookupEnv := deps.run.lookupEnv
		if lookupEnv == nil {
			lookupEnv = os.LookupEnv
		}
		reg := deps.registry()
		warn := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: reg}, LookupEnv: lookupEnv}
		warnings, inviteWarned := secretWarnings(reg, warn)
		for _, w := range warnings {
			_, _ = fmt.Fprintln(stderr, w)
		}
		if err := firstRunForeground(cmd, deps, reg, lookupEnv); err != nil {
			return err
		}
		// The enrollment hook of the foreground Worker must not repeat a
		// warning that was just printed.
		run := deps.run
		run.inviteArgvWarned = inviteWarned
		return runWorkerRun(cmd, run)
	}
	return installWorkerService(cmd, deps, det, asUser)
}

// firstRunForeground is the first-run gate of the foreground mode: it resolves
// the settings like "worker run" does and lets the wizard decide.
func firstRunForeground(cmd *cobra.Command, deps workerUpDeps, reg *config.Registry, lookupEnv func(string) (string, bool)) error {
	sd := deps.setupDeps()
	res := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: reg}, LookupEnv: lookupEnv}
	if sd.loadConfig != nil {
		file, err := loadConfigFile(res, sd.loadConfig)
		if err != nil {
			return err
		}
		res.File = file
	}
	return sd.firstRun(cmd, res, "", "")
}

// installWorkerService is the service path of "worker up": resolve the
// settings for the target user, run the pre-start hooks, save the config file
// and install the service.
func installWorkerService(cmd *cobra.Command, deps workerUpDeps, det service.Detection, asUser string) (err error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	stderr := deps.run.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}

	target, err := service.ResolveTarget(deps.targetEnv, asUser)
	if err != nil {
		return err
	}
	binary, err := resolveBinary(deps.executable)
	if err != nil {
		return err
	}
	lookupEnv := deps.run.lookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	reg := deps.registry()
	flags := changedFlags{cmd: cmd, reg: reg}

	// Where the target user keeps the Worker state. Under sudo the process
	// belongs to root, so the defaults of this process are the wrong ones.
	pe := targetPathEnv(deps, target, lookupEnv)
	defaultState, err := pe.StateDir()
	if err != nil {
		return err
	}
	// What the service resolves without help: a service manager does not carry
	// the environment of the shell that runs this command.
	bare := pe
	bare.Getenv = func(string) string { return "" }
	serviceState, err := bare.StateDir()
	if err != nil {
		return err
	}
	plain := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: flags, LookupEnv: lookupEnv}
	stateRes, err := plain.Resolve(config.KeyStateDir)
	if err != nil {
		return err
	}
	stateDir := defaultState
	if stateRes.Source != config.SourceDefault {
		if stateDir, err = filepath.Abs(stateRes.Value); err != nil {
			return fmt.Errorf("setting %q: %w", config.KeyStateDir, err)
		}
	}
	pinState := filepath.Clean(stateDir) != filepath.Clean(serviceState)

	owned := deps.targetEnv.EUID == 0 && target.UID >= 0 && target.UID != deps.targetEnv.EUID
	st, err := openUpState(stateDir, target, owned, deps)
	if err != nil {
		return err
	}
	defer st.close()

	// A Worker that was never set up gets the wizard (on a terminal) or a
	// clear refusal, before anything is installed or changed. The wizard saves
	// into the same state directory, so everything below sees its answers.
	// The literal-key warning is about this command line only (no file or
	// stdin is read for it), so it comes before the wizard or the refusal.
	warnings, inviteWarned := secretWarnings(reg, &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: flags, LookupEnv: lookupEnv})
	for _, w := range warnings {
		_, _ = fmt.Fprintln(stderr, w)
	}
	if err := firstRunService(cmd, deps, reg, flags, lookupEnv, st, stateDir, target, owned); err != nil {
		return err
	}

	// Settings as the service will see them: this run's flags and environment,
	// the target's own defaults for the per-user paths and the target's config
	// file.
	file, err := st.load()
	if err != nil {
		return err
	}
	withFile := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: flags, File: file, LookupEnv: lookupEnv}
	res := &config.Resolver{
		Registry: reg, Role: config.ScopeWorker, Flags: flags, File: file,
		LookupEnv: withTargetDefaults(lookupEnv, reg, plain, withFile, pe, stateDir),
	}
	level, err := res.Resolve(config.KeyLogLevel)
	if err != nil {
		return err
	}
	format, err := res.Resolve(config.KeyLogFormat)
	if err != nil {
		return err
	}
	logger, err := logx.New(logx.Options{Level: level.Value, Format: logx.Format(format.Value), Output: stderr})
	if err != nil {
		return err
	}
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return err
	}

	// Whatever happens from here on, including a hook or a save that fails
	// after it wrote files as root, the target user must own their state.
	defer func() {
		if cerr := st.give(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if err := runUpHooks(ctx, deps, agent.Settings{WorkerCore: core, Resolver: res, Logger: logger, InviteArgvWarned: inviteWarned}, out); err != nil {
		return err
	}

	// A hook may have written the config file itself: start from what is on disk.
	if file, err = st.load(); err != nil {
		return err
	}
	if err := fillUpConfig(file, plain, reg, flags, lookupEnv); err != nil {
		return err
	}
	path, err := st.save(file)
	if err != nil {
		return err
	}
	if err := st.give(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Settings saved to %s.\n", path)

	svc := deps.service
	svc.Chown = nil
	if owned {
		svc.Chown = deps.chown
	}
	_, err = service.Install(ctx, svc, service.Request{
		Manager:     det.Manager,
		Scope:       det.Scope,
		Binary:      binary,
		Target:      target,
		StateDir:    stateDir,
		PinStateDir: pinState,
	}, out)
	var linger *service.LingerError
	if errors.As(err, &linger) {
		_, _ = fmt.Fprintf(stderr, "Lingering is not enabled for %s. Run this command, then run minion worker up again:\n  %s\n", linger.User, linger.Command)
	}
	return err
}

// firstRunService is the first-run gate of the service path.
func firstRunService(cmd *cobra.Command, deps workerUpDeps, reg *config.Registry, flags changedFlags, lookupEnv func(string) (string, bool), st *upState, stateDir string, target service.Target, owned bool) error {
	file, err := st.load()
	if err != nil {
		return err
	}
	res := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: flags, File: file, LookupEnv: lookupEnv}
	owner := ""
	if owned {
		owner = target.User
	}
	return deps.setupDeps().firstRun(cmd, res, stateDir, owner)
}

func resolveBinary(executable func() (string, error)) (string, error) {
	if executable == nil {
		executable = os.Executable
	}
	p, err := executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine the path of the minion binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}

// targetPathEnv returns the PathEnv of the target user: that user's home and
// UID, and the environment of this process only when the target is the user
// that runs the command (under sudo the environment is root's).
func targetPathEnv(deps workerUpDeps, t service.Target, lookupEnv func(string) (string, bool)) config.PathEnv {
	pe := config.SystemPathEnv()
	pe.HomeDir = func() (string, error) { return t.Home, nil }
	pe.UID = t.UID
	if t.UID != deps.targetEnv.EUID {
		pe.Getenv = func(string) string { return "" }
	} else if deps.run.lookupEnv != nil {
		pe.Getenv = func(k string) string { v, _ := lookupEnv(k); return v }
	}
	return pe
}

// withTargetDefaults wraps lookupEnv so that the settings whose default
// belongs to a user (the state directory and the tmux socket) resolve to the
// target user's default when nothing else sets them. plain resolves from flags
// and environment only; withFile adds the target's config file, so that a
// value saved there is not shadowed by a synthetic default. An empty variable
// counts as unset, exactly as the Resolver reads it.
func withTargetDefaults(lookupEnv func(string) (string, bool), reg *config.Registry, plain, withFile *config.Resolver, pe config.PathEnv, stateDir string) func(string) (string, bool) {
	inject := map[string]string{}
	if s, ok := reg.Lookup(config.KeyStateDir); ok {
		if r, err := plain.Resolve(s.Key); err == nil && r.Source == config.SourceDefault {
			inject[s.EnvName()] = stateDir
		}
	}
	if s, ok := reg.Lookup(config.KeyTmuxSocket); ok {
		if r, err := withFile.Resolve(s.Key); err == nil && r.Source == config.SourceDefault {
			if sock, err := pe.TmuxSocket(); err == nil && sock != "" {
				inject[s.EnvName()] = sock
			}
		}
	}
	return func(k string) (string, bool) {
		v, ok := lookupEnv(k)
		if ok && v != "" {
			return v, true
		}
		if inj, has := inject[k]; has {
			return inj, true
		}
		return v, ok
	}
}

// secretWarnings returns the process-list warnings of the secret settings that
// were given literally on the command line (R85), and whether one of them is
// the invitation key (so that the enrollment hook does not warn again).
//
// The invitation key is special: --invite-key also takes a file path or "-"
// (standard input), and neither puts a secret into argv. It is warned about
// only when the value is the literal key (config.InviteKeyGiven looks at the
// flag value's form and never reads a file or standard input), whether or not
// this Worker is already enrolled and the key will be used.
func secretWarnings(reg *config.Registry, res *config.Resolver) (warnings []string, inviteWarned bool) {
	for _, s := range reg.ForScope(config.ScopeWorker) {
		if !s.Secret || !s.HasFlag() {
			continue
		}
		if s.Key == config.KeyInviteKey {
			// Classify by form only, before anything is resolved: Resolve
			// would read MINION_INVITE_KEY_FILE (which may block) just to
			// find out there is nothing to warn about. A literal flag value
			// is resolved from the flag alone, so Resolve below reads no file.
			if _, literal, gerr := config.InviteKeyGiven(res); gerr != nil || !literal {
				continue
			}
			inviteWarned = true
		}
		r, err := res.Resolve(s.Key)
		if err != nil || r.Warning == "" {
			continue
		}
		warnings = append(warnings, r.Warning)
	}
	return warnings, inviteWarned
}

// runUpHooks runs the registered pre-start hooks once and reports the result
// to out. A failing hook aborts, naming the hook.
func runUpHooks(ctx context.Context, deps workerUpDeps, st agent.Settings, out io.Writer) error {
	reg := deps.run.registry
	if reg == nil {
		reg = agent.DefaultRegistry
	}
	names := reg.PreStartNames()
	if len(names) == 0 {
		_, _ = fmt.Fprintln(out, "No pre-start hooks to run.")
		return nil
	}
	st.Out = out
	if err := reg.RunPreStart(ctx, st); err != nil {
		return fmt.Errorf("%w; no service was installed or changed", err)
	}
	_, _ = fmt.Fprintf(out, "Pre-start hooks completed: %s.\n", strings.Join(names, ", "))
	return nil
}

// fillUpConfig puts the non-secret settings that this run gave by flag or
// environment into the config file. A setting that nobody set is left alone,
// so that later changes of a default still apply.
func fillUpConfig(file *config.ConfigFile, plain *config.Resolver, reg *config.Registry, flags changedFlags, lookupEnv func(string) (string, bool)) error {
	for _, s := range reg.ForScope(config.ScopeWorker) {
		if s.Secret || s.Key == config.KeyStateDir {
			continue
		}
		_, byFlag, _ := flags.Lookup(s.Key)
		v, byEnv := lookupEnv(s.EnvName())
		if !byFlag && (!byEnv || v == "") {
			continue
		}
		r, err := plain.Resolve(s.Key)
		if err != nil {
			return err
		}
		val := r.Value
		if s.Key == config.KeyTmuxSocket && val != "" {
			// The service starts in the home directory: save the path the
			// hooks used, which is absolute against this process's directory.
			if val, err = filepath.Abs(val); err != nil {
				return fmt.Errorf("setting %q: %w", s.Key, err)
			}
		}
		file.Set(s.Key, val)
	}
	return nil
}

// newWorkerServiceRunCmd is the entry point of the Windows service: the
// command the service control manager runs. It is "worker run" under the
// service handshake (see [service.SCM]); started from a console it runs like
// "worker run". It is hidden because nobody types it.
func newWorkerServiceRunCmd(deps workerRunDeps, scm service.SCM) *cobra.Command {
	cmd := &cobra.Command{
		Use:    service.WindowsServiceCommand,
		Short:  "Run the Worker under the Windows service control manager",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return scm.Run(ctx, "", func(ctx context.Context) error {
				cmd.SetContext(ctx)
				return runWorkerRun(cmd, deps)
			})
		},
	}
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}
