package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
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
	// windowsService looks at the Worker's Windows service, query only; nil
	// means service.QueryWindowsService (nothing off Windows).
	windowsService func() (service.WindowsServiceState, error)
	// windows holds the seams of the Windows path (Run entry, supervisor);
	// nil means the real machine.
	installLogin func(context.Context, string, string, io.Writer) error
	// liveClient asks a running Worker of this user before a migration; nil
	// skips that check (tests).
	liveClient func() client.Backend
	windows    *windowsUpDeps
	// readStatus asks the newly started Worker for its onboarding summary.
	ownerHooks     func(context.Context, *ownerSetup, agent.Settings, io.Reader, io.Writer) error
	readStatus     upStatusReader
	stopForeground func(context.Context, string, *ownerSetup) error
}

func (d workerUpDeps) windowsDeps() windowsUpDeps {
	if d.windows != nil {
		return *d.windows
	}
	return defaultWindowsUpDeps()
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
		setup:          workerSetupDeps{checkTerminal: checkTerminalHost},
		run:            defaultWorkerRunDeps(stderr),
		detectEnv:      service.SystemEnv(),
		targetEnv:      service.SystemTargetEnv(),
		executable:     os.Executable,
		installLogin:   installLoginWorker,
		liveClient:     func() client.Backend { return client.Local() },
		service:        withEnv(service.DefaultDeps()),
		chown:          service.OSChowner{},
		stopForeground: stopForegroundForService,
		readStatus:     readUpStatus,
		ownerHooks:     runOwnerHooks,
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
	var foreground, configureOnly bool
	var asUser string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Set up the Worker, start it in the background, and enable startup at boot or login",
		Long: `Provision this machine as a Minion Worker with one command.

On Linux with systemd, run with sudo: the Worker is installed as the system
service minion-worker@<user>.service (User=<user>, Restart=always, ExecStart=
<binary> worker run), enabled at boot and started now. Without sudo a systemd
user service is installed instead; the command then enables lingering for you
(loginctl enable-linger) or, when that is not allowed, prints the exact command
to run and exits with an error. On macOS a launchd agent is installed and
loaded for the current user. On Windows the Worker runs as you: a value in
your Run key (HKCU) starts its supervisor ("minion worker launch") at each
logon, without a console window, and it is started now; no administrator is
needed and no Windows service is installed. Run it in a normal terminal, not
as administrator. A LocalSystem service MinionWorker of an older release is
removed first (that needs an administrator).

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
and a pairing code. The Hub join command supplies the Hub and invitation.
On a fresh unattended installation it uses --non-interactive-share all,
sharing every session (change it in the Hub). Other unattended commands
need an explicit sharing choice. Under sudo the sharing question lets you
choose the invoking user's sessions or run the Worker as root. In a terminal, a
Worker that is about to pair still asks for the Hub fingerprint.

The quickest way to join a Hub is the join token from the Hub's "Add worker"
page: --join mnj1-... (or MINION_JOIN, or MINION_JOIN_FILE with the path of a
file that holds it). It holds the Hub URL, the full Hub fingerprint and a
one-time invitation key, and sets --server, --hub-fingerprint and
--invite-key: the Worker connects only to the Hub with that fingerprint and
enrolls with the key, which works once and expires. Giving one of these three
with a different value is an error. The Hub address and fingerprint are saved
for the service; the token itself is never saved or printed. --join - reads
the token from the first line of standard input (at most 4096 bytes), so that it
is in no argument list; sharing behaves exactly as with --join <token>.

"worker up" first runs the pre-start hooks (for example invitation
enrollment) and reports their result. A failing hook stops the command with an
error before any service is installed or changed.

On Linux without systemd, the Worker starts in the background, writes to
worker-launch.log in its state directory and starts again at shell login.
Use "worker down" to stop it and "worker start" to start it again.
With --foreground it runs on this terminal until SIGINT or SIGTERM.
For Docker, use --configure-only to enroll once on a persistent state volume.
Run "minion worker run" in the container with Docker's restart policy enabled.
Mount the volume at /data and set MINION_STATE_DIR=/data. To enroll from an
invitation file, set MINION_SERVER to the Hub URL and MINION_INVITE_KEY_FILE
to the mounted file during configuration.`,
		Args: noArgsRedacted,
		RunE: withHubLine(func(cmd *cobra.Command, _ []string) error {
			return runWorkerUp(cmd, deps, foreground, configureOnly, asUser)
		}, deps.run.lookupEnv),
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Run the Worker in the foreground instead of installing a service")
	cmd.Flags().BoolVar(&configureOnly, "configure-only", false, "Enroll and save settings for an external supervisor such as Docker, then exit")
	cmd.MarkFlagsMutuallyExclusive("foreground", "configure-only")
	cmd.Flags().String("non-interactive-share", "", "Initial sharing choice (all, none or later) for an unattended Worker with no saved choice")
	cmd.Flags().StringVar(&asUser, "as-user", "", "Install the service for this Unix user (needs root; default: the user that ran sudo)")
	addSettingFlags(cmd, deps.registry(), config.ScopeWorker)
	addJoinFlag(cmd, deps.registry())
	return cmd
}

func runWorkerUp(cmd *cobra.Command, deps workerUpDeps, foreground, configureOnly bool, asUser string) error {
	// First of all, before a join token is expanded or any setup runs: the
	// Worker never runs as LocalSystem (it would run the user's binary as
	// SYSTEM); LocalSystem has no UAC split token, so the token SID is read.
	if err := service.RefuseSystem(systemAccount()); err != nil {
		return err
	}
	// A join token becomes --server, --hub-fingerprint and --invite-key before
	// anything else looks at the settings (worker_join_token.go). Its
	// process-list warning comes with the other secret warnings below.
	upEnv := deps.run.lookupEnv
	if upEnv == nil {
		upEnv = os.LookupEnv
	}
	join, err := expandJoinToken(cmd, deps.registry(), upEnv)
	if err != nil {
		return err
	}
	if join.Warning != "" {
		w := deps.run.stderr
		if w == nil {
			w = cmd.ErrOrStderr()
		}
		_, _ = fmt.Fprintln(w, join.Warning)
	}
	det := service.Detect(deps.detectEnv)
	if foreground {
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
		warnWindowsService(stderr, deps.windowsService)
		if err := firstRunForeground(cmd, deps, reg, lookupEnv); err != nil {
			return err
		}
		// The enrollment hook of the foreground Worker must not repeat a
		// warning that was just printed.
		run := deps.run
		run.inviteArgvWarned = inviteWarned
		return runWorkerRun(cmd, run)
	}
	err = installWorkerService(cmd, deps, det, asUser, configureOnly)
	if errors.Is(err, errRunAsRoot) {
		// Chosen in the sharing question: the Worker runs as root (A57). A
		// state directory given for the user would become root's: refused.
		if cmd.Flags().Changed("state-dir") || os.Getenv("MINION_STATE_DIR") != "" || os.Getenv("XDG_STATE_HOME") != "" {
			return errors.New("to run the Worker as root, run the command again without --state-dir, MINION_STATE_DIR and XDG_STATE_HOME (they can name a directory of the user); sudo -i gives a clean root environment")
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "The Worker will run as root and serve root's tmux sessions.")
		return installWorkerService(cmd, deps, det, "root", configureOnly)
	}
	return err
}

// warnWindowsService warns a foreground Worker while the LocalSystem Windows
// service of an older release exists, with the commands that remove it. It
// only queries the service control manager; a failed query is reported as unknown state.
func warnWindowsService(w io.Writer, query func() (service.WindowsServiceState, error)) {
	if query == nil {
		query = service.QueryWindowsService
	}
	st, err := query()
	if err != nil {
		_, _ = fmt.Fprintln(w, "Warning: "+service.LegacyQueryFailure(err))
		return
	}
	if msg := service.LegacyServiceWarning(st); msg != "" {
		_, _ = fmt.Fprintln(w, "Warning: "+msg)
	}
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
		// A Hub pin saved in the config file beforehand is never replaced
		// by a join token: checked before the wizard or anything else.
		if err := joinFileConflict(cmd, reg, file); err != nil {
			return err
		}
	}
	if core, err := config.ResolveWorkerCore(res); err == nil {
		if err := refuseJoinWhilePaused(cmdContext(cmd), cmd, &upState{dir: core.StateDir}); err != nil {
			return err
		}
	}
	return sd.firstRun(cmd, res, nil, nil)
}

// installWorkerService is the service path of "worker up": resolve the
// settings for the target user, run the pre-start hooks, save the config file
// and install the service.
func installWorkerService(cmd *cobra.Command, deps workerUpDeps, det service.Detection, asUser string, configureOnly bool) (err error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	stderr := deps.run.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}

	// Windows: an older LocalSystem service goes, and an elevated prompt is
	// refused, before anything is saved or changed.
	if det.Manager == service.ManagerWindows {
		if err := windowsPreflight(ctx, deps.windowsDeps(), out, !configureOnly); err != nil {
			var na *needsAdminError
			if errors.As(err, &na) {
				_, _ = fmt.Fprintln(stderr, "Error: "+na.msg)
				return &exitCodeError{ExitNeedsAdmin}
			}
			return err
		}
	}
	target, err := service.ResolveTarget(deps.targetEnv, asUser)
	if err != nil {
		return err
	}
	binary, err := resolveBinary(deps.executable)
	if err != nil {
		return err
	}
	// A Worker of root runs this binary as root at every start: only root may
	// be able to change it (checked before anything is stopped or saved).
	if runningAsRoot() && target.UID == 0 && det.Manager != service.ManagerWindows && !configureOnly {
		if err := checkRootExecutable(binary); err != nil {
			return err
		}
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
	// The target's saved Hub pin wins over a join token: a different one
	// stops here, before the wizard, the hooks or any write.
	if saved, err := st.load(); err != nil {
		return err
	} else if err := joinFileConflict(cmd, reg, saved); err != nil {
		return err
	}
	if err := refuseJoinWhilePaused(ctx, cmd, st); err != nil {
		return err
	}

	// A Worker that was never set up gets the wizard (on a terminal) or a
	// clear refusal, before anything is installed or changed. The wizard saves
	// into the same state directory, so everything below sees its answers.
	// The literal-key warning is about this command line only (no file or
	// stdin is read for it), so it comes before the wizard or the refusal.
	warnings, inviteWarned := secretWarnings(reg, &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: flags, LookupEnv: lookupEnv})
	for _, w := range warnings {
		_, _ = fmt.Fprintln(stderr, w)
	}
	var owner *ownerSetup
	if owned {
		sock, err := pe.TmuxSocket()
		if err != nil {
			return err
		}
		if f, err := st.load(); err == nil {
			r := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: flags, File: f, LookupEnv: lookupEnv}
			if v, err := r.Resolve(config.KeyTmuxSocket); err == nil && v.Source != config.SourceDefault && v.Value != "" {
				sock = v.Value
			}
		}
		owner = &ownerSetup{Target: target, StateDir: stateDir, Socket: sock, Binary: binary}
	}
	if err := firstRunService(cmd, deps, reg, flags, lookupEnv, st, owner); err != nil {
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
	logger, err := setupLogger(cmd, res, stderr)
	if err != nil {
		return err
	}
	// The service sees only the config file (and the variables the unit
	// pins), never this shell's OTEL_EXPORTER_OTLP_* variables: check the
	// log sinks as the service will resolve them, before any hook, save or
	// restart, so that a typo cannot put a working Worker into a restart
	// loop.
	if err := checkServiceSinks(res, serviceSinkReader(owned, target.UID, target.GID)); err != nil {
		return fmt.Errorf("%w; nothing was saved and the service was not changed", err)
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

	hookSettings := agent.Settings{WorkerCore: core, Resolver: res, Logger: logger, InviteArgvWarned: inviteWarned}
	if owned && deps.ownerHooks != nil {
		if err := st.give(); err != nil {
			return err
		}
		err = deps.ownerHooks(ctx, owner, hookSettings, cmd.InOrStdin(), out)
	} else {
		err = runUpHooks(ctx, deps, hookSettings, out)
	}
	if err != nil {
		return err
	}

	// A hook may have written the config file itself: start from what is on disk.
	// Under the config lock of "worker config set", from the reload to the
	// save: a change saved meanwhile (allow_new_sessions off, for example) is
	// kept, never written over with the older file.
	// A join that an earlier run left half done (killed between the
	// enrollment commit and this save) is finished from the enrollment.
	recovered := recoverJoinedHub(ctx, st)
	path, err := st.update(func(file *config.ConfigFile) error {
		if err := fillUpConfig(file, plain, reg, flags, lookupEnv); err != nil {
			return err
		}
		fillRecoveredHub(file, recovered)
		return nil
	})
	if err != nil {
		return err
	}
	if err := st.give(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Settings saved to %s.\n", path)
	if configureOnly {
		return nil
	}

	// All background managers use the same identity and readiness summary.
	summary := func(ctx context.Context) error {
		if deps.readStatus == nil {
			return nil
		}
		commands := upNextCommands(det, target, owner != nil, pinState, stateDir)
		return showUpStatus(ctx, out, stateDir, owner, deps.readStatus, commands)
	}

	if !owned && deps.liveClient != nil {
		hub, _ := config.ResolveWorkerHub(res)
		if err := checkMigrationKeepsHub(ctx, stateDir, hub.Server != "", deps.liveClient()); err != nil {
			return err
		}
	}
	svc := deps.service
	svc.Chown = nil
	if owned {
		svc.Chown = deps.chown
	}
	if det.Manager == service.ManagerWindows {
		// The Run entry always names the state directory chosen now, so a
		// MINION_STATE_DIR in the environment of a later logon (or of this
		// shell) cannot select another profile.
		return installWindowsAutostart(ctx, deps.windowsDeps(), binary, stateDir, true, out, summary)
	}
	if det.Manager == service.ManagerNone {
		// Share one deadline between binding the socket and waiting for the
		// Hub welcome so an unreachable Hub cannot hold the shell open.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if owned || (target.UID == 0 && asUser == "root" && target.Home != os.Getenv("HOME")) {
			err = runAsUser(ctx, target, binary, []string{"worker", "install-login", stateDir}, out)
		} else {
			err = deps.installLogin(ctx, binary, stateDir, out)
		}
	} else {
		if deps.stopForeground != nil {
			if err := deps.stopForeground(ctx, stateDir, owner); err != nil {
				return err
			}
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
	}
	if err != nil {
		return err
	}
	// The Worker runs now; a status that cannot be shown is not a failure of
	// the setup (the one-time invitation is already used).
	if err := summary(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "The Worker was started, but its state could not be shown (%v). Check it with: minion worker status\n", err)
	}
	return nil
}

// recoverJoinedHub reads the Hub of a committed enrollment (see
// agent.Enrollments.RecoverHub). It never fails the command: a state that
// cannot be read gives no recovery. Root working on another user's directory
// is skipped; the Worker recovers the address itself at its next start.
func recoverJoinedHub(ctx context.Context, st *upState) *agent.RecoveredHub {
	if st.root != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(st.dir, store.DBFile)); err != nil {
		return nil
	}
	ro, err := store.OpenReadOnlyContext(ctx, st.dir)
	if err != nil {
		return nil
	}
	defer func() { _ = ro.Close() }()
	rec, ok, err := agent.NewEnrollments(ro, st.dir).RecoverHub(ctx)
	if err != nil || !ok {
		return nil
	}
	return &rec
}

// fillRecoveredHub saves the Hub of a committed enrollment where the config
// file names none: an address or fingerprint that is already set is kept.
func fillRecoveredHub(file *config.ConfigFile, rec *agent.RecoveredHub) {
	if rec == nil {
		return
	}
	if v, ok, _ := file.Lookup(config.KeyServer); !ok || v == "" {
		file.Set(config.KeyServer, rec.URL)
		if v, ok, _ := file.Lookup(config.KeyHubFingerprint); (!ok || v == "") && rec.Fingerprint != "" {
			file.Set(config.KeyHubFingerprint, rec.Fingerprint)
		}
	}
}

// firstRunService is the first-run gate of the service path.
func firstRunService(cmd *cobra.Command, deps workerUpDeps, reg *config.Registry, flags changedFlags, lookupEnv func(string) (string, bool), st *upState, owner *ownerSetup) error {
	file, err := st.load()
	if err != nil {
		return err
	}
	res := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: flags, File: file, LookupEnv: lookupEnv}
	return deps.setupDeps().firstRun(cmd, res, st, owner)
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
	managedAndroid := false
	if m, err := withFile.Resolve(config.KeyManagedHost); err == nil && m.Value == config.ManagedHostAndroid {
		managedAndroid = true // the private socket is never a default
	}
	if s, ok := reg.Lookup(config.KeyTmuxSocket); ok && !managedAndroid {
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
		if !s.Secret || !s.HasFlag() || s.Key == config.KeyJoin {
			// The join token was read once, by expandJoinToken, which
			// returned its warning: reading MINION_JOIN_FILE again could
			// block on a pipe.
			continue
		}
		if s.Key == config.KeyInviteKey && joinApplied(res) {
			// The key came from a join token (--join warns for itself, and
			// MINION_JOIN puts nothing into argv): no warning, and none from
			// the enrollment hook either.
			inviteWarned = true
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

// joinApplied reports whether the flags of res are those of a command whose
// join token was expanded.
func joinApplied(res *config.Resolver) bool {
	cf, ok := res.Flags.(changedFlags)
	return ok && cf.cmd != nil && cf.cmd.Annotations[joinAppliedAnnotation] != ""
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

// servicePathKeys are the file settings of the log sinks: relative paths
// are saved absolute.
var servicePathKeys = map[string]bool{
	config.KeyOTelHeadersPath: true, config.KeySyslogCAFile: true, config.KeySyslogClientCertFile: true,
	config.KeySyslogClientKeyFile: true, config.KeyOTelCAFile: true, config.KeyOTelClientCertFile: true,
	config.KeyOTelClientKeyFile: true,
}

// checkServiceSinks resolves the log sinks exactly as the installed service
// will: the settings of this run that worker up saves (flags and MINION_*
// variables, file paths trimmed and made absolute as they are saved) and the
// config file, without other variables of this shell (an
// OTEL_EXPORTER_OTLP_ENDPOINT here does not reach the service). A relative
// path already in the config file is refused: the service opens it from its
// own directory. Private files are read with the daemon's rules (read).
func checkServiceSinks(res *config.Resolver, read func(string) ([]byte, error)) error {
	svc := *res
	lookup := res.LookupEnv
	svc.LookupEnv = func(name string) (string, bool) {
		if !strings.HasPrefix(name, config.EnvPrefix) || lookup == nil {
			return "", false
		}
		return lookup(name)
	}
	over := map[string]string{}
	for key := range servicePathKeys {
		r, err := svc.Resolve(key)
		if err != nil {
			return fmt.Errorf("log export: %w", err)
		}
		v := strings.TrimSpace(r.Value)
		if v == "" {
			continue
		}
		switch r.Source {
		case config.SourceFlag, config.SourceEnv:
			abs, err := filepath.Abs(v)
			if err != nil {
				return fmt.Errorf("log export: setting %q: %w", key, err)
			}
			over[key] = abs
		case config.SourceFile:
			if !filepath.IsAbs(v) {
				return fmt.Errorf("log export: setting %q in the config file is a relative path, which the service resolves from its own directory; set an absolute path", key)
			}
		}
	}
	svc.Flags = overlayFlags{base: res.Flags, over: over}
	sinks, err := config.ResolveSinks(&svc, config.SinkFiles{ReadPrivate: read})
	if err == nil {
		_, err = config.ResolveTraffic(&svc, sinks)
	}
	if err != nil {
		return fmt.Errorf("log export: %w (the service does not see OTEL_EXPORTER_OTLP_* variables of this shell; use --otel-endpoint)", err)
	}
	return nil
}

// overlayFlags is a flag source with some values replaced.
type overlayFlags struct {
	base config.FlagSource
	over map[string]string
}

func (o overlayFlags) Lookup(key string) (string, bool, error) {
	if v, ok := o.over[key]; ok {
		return v, true, nil
	}
	if o.base == nil {
		return "", false, nil
	}
	return o.base.Lookup(key)
}

// serviceSinkReader reads a private sink file with the rules of the Worker
// daemon (identity.OpenPrivate: no link, a regular file of the user, no
// access for others). When root installs the service of another account,
// the file must belong to that account instead (serviceOwnedPrivate).
func serviceSinkReader(owned bool, uid, gid int) func(string) ([]byte, error) {
	if !owned {
		return readPrivateSinkFile
	}
	return func(path string) ([]byte, error) { return serviceOwnedPrivate(path, uid, gid) }
}

// managedHostKeys are given by the supervisor (the Android app) through the
// environment at every launch and never saved: an APK update moves the
// bundled tmux binary, and a saved path would go stale.
var managedHostKeys = map[string]bool{config.KeyTmuxBinary: true, config.KeyManagedHost: true, config.KeyTmuxConfig: true}

// fillUpConfig puts the non-secret settings that this run gave by flag or
// environment into the config file. A setting that nobody set is left alone,
// so that later changes of a default still apply.
func fillUpConfig(file *config.ConfigFile, plain *config.Resolver, reg *config.Registry, flags changedFlags, lookupEnv func(string) (string, bool)) error {
	for _, s := range reg.ForScope(config.ScopeWorker) {
		// Only a value given by flag or environment is saved; a saved
		// allow_new_sessions is never overwritten otherwise.
		if s.Secret || s.Key == config.KeyStateDir || managedHostKeys[s.Key] {
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
		if r.Source == config.SourceDefault {
			continue // A blank override can resolve as unset.
		}
		val := r.Value
		if servicePathKeys[s.Key] {
			val = strings.TrimSpace(val)
		}
		if servicePathKeys[s.Key] && val != "" {
			// The service starts in another directory (the home of its
			// user): save the path this run resolved.
			if val, err = filepath.Abs(val); err != nil {
				return fmt.Errorf("setting %q: %w", s.Key, err)
			}
		}
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

// newWorkerServiceRunCmd is the command the LocalSystem service of releases up
// to 0.13.1 runs. It no longer runs the Worker: under the service handshake
// (see [service.SCM]) it reports a clean stop, so the service is not restarted,
// and says how to remove the service. It is hidden because nobody types it.
func newWorkerServiceRunCmd(deps workerRunDeps, scm service.SCM) *cobra.Command {
	cmd := &cobra.Command{
		Use:    service.WindowsServiceCommand,
		Short:  "End a Windows service of an older release (the Worker now runs as the user)",
		Hidden: true,
		Args:   noArgsRedacted,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Releases up to 0.13.1 installed the Worker as the LocalSystem
			// service MinionWorker with this entry point. It no longer runs
			// the Worker as SYSTEM: it reports to the service control manager
			// that it stopped cleanly (so the restart-on-failure action does
			// not loop) and says what to do.
			return scm.End(func() {
				w := deps.stderr
				if w == nil {
					w = cmd.ErrOrStderr()
				}
				_, _ = fmt.Fprintln(w, legacyServiceRunMessage)
			})
		},
	}
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}

// legacyServiceRunMessage is what "worker service-run" says instead of running
// the Worker as SYSTEM.
const legacyServiceRunMessage = "minion no longer runs the Worker as the LocalSystem service " + service.WindowsServiceName +
	"; the service ends. Remove it (" + service.LegacyRemovalCommand + ") and run minion worker up as yourself."

// refuseJoinWhilePaused stops a join token on a paused Worker that is not
// enrolled yet: a paused Worker makes no Hub request, so the invitation would
// not be checked, and the command would report a join that did not happen.
func refuseJoinWhilePaused(ctx context.Context, cmd *cobra.Command, st *upState) error {
	if cmd.Annotations[joinAppliedAnnotation] == "" {
		return nil
	}
	if paused, _ := agent.ReadPause(st.dir); !paused {
		return nil
	}
	if enrolled, err := st.enrolled(ctx); err != nil || enrolled {
		return err
	}
	return errors.New("this Worker is paused, so it makes no Hub request and cannot use the join token; nothing was changed. " +
		"Run minion worker resume, then run this command again with a fresh token")
}
