package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func init() {
	Register(func(root *cobra.Command) {
		worker := workerCommand(root)
		worker.AddCommand(newWorkerConfigCmd(defaultWorkerConfigDeps()))
	})
}

// configChange is one change of the Worker config that a running Worker may
// be asked to apply.
type configChange struct {
	Key   string
	Value string
	// Unset is true when the key was removed from the config file.
	Unset bool
}

// configApplier hands a saved change to a running Worker (over its local
// socket, AD-22). It returns [agent.ErrNoLiveWorker] when none runs or none
// can be reached; the change is then applied at the next start.
type configApplier interface {
	ApplyConfig(ctx context.Context, stateDir string, change configChange) error
}

// noLiveApplier is the applier used until the Worker's local socket has an op
// for config changes (Story 2.8 serves the socket, but only the Worker ops):
// it never contacts a Worker.
type noLiveApplier struct{}

func (noLiveApplier) ApplyConfig(context.Context, string, configChange) error {
	return agent.ErrNoLiveWorker
}

// shareStore is the part of the Worker state that "config share" uses;
// *store.Store implements it.
type shareStore interface {
	LoadShareContext(ctx context.Context) (store.ShareSelection, error)
	SaveShareContext(ctx context.Context, sel store.ShareSelection) error
	Close() error
}

// workerConfigDeps are the inputs of "minion worker config" that tests replace.
type workerConfigDeps struct {
	lookupEnv func(string) (string, bool)
	openStore func(ctx context.Context, stateDir string) (shareStore, error)
	applier   configApplier
	// registry is the settings registry; nil means config.Default.
	registry *config.Registry
}

func (d workerConfigDeps) reg() *config.Registry {
	if d.registry != nil {
		return d.registry
	}
	return config.Default
}

func defaultWorkerConfigDeps() workerConfigDeps {
	return workerConfigDeps{
		lookupEnv: os.LookupEnv,
		openStore: func(ctx context.Context, dir string) (shareStore, error) { return store.OpenContext(ctx, dir) },
		applier:   noLiveApplier{},
	}
}

func newWorkerConfigCmd(deps workerConfigDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Change this Worker's settings after setup",
		Long: `Change this Worker's settings without starting setup over.

  minion worker config set server https://hub.example:8443
  minion worker config set <key> <value>
  minion worker config unset <key>
  minion worker config share all|none|later
  minion worker config share select <session>...

Settings are saved in the config file in the state directory and in the Worker
state database (the share choice). A running Worker reads the share choice from
its state database, so that change takes effect at once. Any other saved value
is used the next time the Worker starts; restart a Worker that already runs (for
example the service) to pick it up. A flag or an environment variable still
wins over the file, and the command says so when it applies.

Secrets are never accepted on the command line. Put a secret in a file and set
the matching MINION_<NAME>_FILE variable. The one exception is the invitation
key (--invite-key), which is meant for one-command setup.`,
	}
	cmd.AddCommand(
		newConfigSetCmd(deps),
		newConfigUnsetCmd(deps),
		newConfigShareCmd(deps),
	)
	return cmd
}

// configEnv is what one config command works on: the writable config file, the
// state directory and the way to report. It holds the config lock until
// [configEnv.release] is called.
type configEnv struct {
	stateDir string
	file     *config.ConfigFile
	out      io.Writer
	errOut   io.Writer
	release  func()
}

// configLockName is the lock file in the state directory that serializes the
// config commands of every process, from reading the file to saving it, so
// that two commands never overwrite each other's change.
const configLockName = ".config.lock"

// writef writes formatted text and returns the write error, so that a closed
// pipe is not reported as success.
func writef(w io.Writer, format string, a ...any) error {
	_, err := fmt.Fprintf(w, format, a...)
	return err
}

func (d workerConfigDeps) env(cmd *cobra.Command) (*configEnv, error) {
	res := &config.Resolver{
		Role:      config.ScopeWorker,
		Registry:  d.reg(),
		Flags:     changedFlags{cmd: cmd, reg: d.reg()},
		LookupEnv: d.lookupEnv,
	}
	dir, err := res.Resolve(config.KeyStateDir)
	if err != nil {
		return nil, err
	}
	if dir.Value == "" {
		return nil, fmt.Errorf("the state directory is not known; give --%s or set %s", config.KeyStateDir, "MINION_STATE_DIR")
	}
	abs, err := filepath.Abs(dir.Value)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the state directory %q: %w", abs, err)
	}
	release, err := lockConfigFile(filepath.Join(abs, configLockName))
	if err != nil {
		return nil, err
	}
	// The file is read only now, under the lock: a command that ran before us
	// has saved its change by the time we load.
	file, err := config.LoadFile(filepath.Join(abs, workerConfigName))
	if err != nil {
		release()
		return nil, err
	}
	return &configEnv{stateDir: abs, file: file, out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr(), release: release}, nil
}

func newConfigSetCmd(deps workerConfigDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Save one setting, for example: set server https://hub.example:8443",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigSet(cmd, deps, args[0], args[1])
		},
	}
	addNamedSettingFlags(cmd, deps.reg(), config.KeyStateDir)
	return cmd
}

func newConfigUnsetCmd(deps workerConfigDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset <key>",
		Short: "Remove one setting from the config file, for example: unset server runs the Worker alone (Solo)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigUnset(cmd, deps, args[0])
		},
	}
	addNamedSettingFlags(cmd, deps.reg(), config.KeyStateDir)
	return cmd
}

func newConfigShareCmd(deps workerConfigDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share all|none|later | select <session>...",
		Short: "Choose which tmux sessions the Hub may see",
		Long: `Choose which tmux sessions the Hub may see.

  all      share every session, also sessions created later
  none     share no session
  select   share exactly the named sessions
  later    decide later (for example from the Hub); nothing is shared until then

"select" without names keeps the sessions chosen before.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigShare(cmd, deps, args[0], args[1:])
		},
	}
	addNamedSettingFlags(cmd, deps.reg(), config.KeyStateDir)
	return cmd
}

// workerSetting looks up key and checks that it is a Worker setting that this
// command may change.
func workerSetting(reg *config.Registry, key string) (config.Setting, error) {
	s, ok := reg.Lookup(key)
	if ok && s.Scope != config.ScopeWorker && s.Scope != config.ScopeAny {
		ok = false
	}
	if !ok {
		return config.Setting{}, fmt.Errorf("unknown setting %q; the Worker settings are: %s", key, strings.Join(workerSettingKeys(reg), ", "))
	}
	if s.Key == config.KeyStateDir {
		return config.Setting{}, fmt.Errorf("%s names the directory that holds the config file, so it cannot be stored in it; give --%s or set %s",
			s.Key, s.FlagName(), s.EnvName())
	}
	return s, nil
}

func workerSettingKeys(reg *config.Registry) []string {
	var keys []string
	for _, s := range reg.ForScope(config.ScopeWorker) {
		if s.Key != config.KeyStateDir && (!s.Secret || s.ArgvAllowedWithWarning) {
			keys = append(keys, s.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

func runConfigSet(cmd *cobra.Command, deps workerConfigDeps, key, value string) error {
	s, err := workerSetting(deps.reg(), key)
	if err != nil {
		return err
	}
	if s.Secret && !s.ArgvAllowedWithWarning {
		return fmt.Errorf("%s is a secret and is never accepted on the command line (argv is visible in the process list); "+
			"put it in a file with mode 0600 and set %s to the path of that file", s.Key, s.FileEnvName())
	}
	if s.Key == config.KeyShareExisting {
		if value == "select" {
			return errors.New("share_existing takes all, none or later; use `minion worker config share select <session>...` to choose sessions")
		}
		return runConfigShare(cmd, deps, value, nil)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("the value is empty; use `minion worker config unset %s` to remove the setting", s.Key)
	}
	ce, err := deps.env(cmd)
	if err != nil {
		return err
	}
	defer ce.release()
	value, err = validateSettingValue(deps.reg(), s, value, ce.stateDir)
	if err != nil {
		return err
	}
	if s.Secret {
		if err := writef(ce.errOut, "warning: the value of %s is visible in the process list while this command runs\n", s.Key); err != nil {
			return err
		}
	}
	ce.file.Set(s.Key, value)
	if err := ce.file.Save(); err != nil {
		return err
	}
	if err := writef(ce.out, "Saved %s in %s.\n", s.Key, ce.file.Path()); err != nil {
		return err
	}
	if err := deps.reportOverride(ce, s); err != nil {
		return err
	}
	return deps.apply(cmd.Context(), ce, configChange{Key: s.Key, Value: value})
}

func runConfigUnset(cmd *cobra.Command, deps workerConfigDeps, key string) error {
	s, err := workerSetting(deps.reg(), key)
	if err != nil {
		return err
	}
	ce, err := deps.env(cmd)
	if err != nil {
		return err
	}
	defer ce.release()
	if _, set, _ := ce.file.Lookup(s.Key); !set {
		return writef(ce.out, "%s is not set in %s; nothing to remove.\n", s.Key, ce.file.Path())
	}
	ce.file.Delete(s.Key)
	if err := ce.file.Save(); err != nil {
		return err
	}
	if err := writef(ce.out, "Removed %s from %s; the default applies.\n", s.Key, ce.file.Path()); err != nil {
		return err
	}
	if err := deps.reportOverride(ce, s); err != nil {
		return err
	}
	return deps.apply(cmd.Context(), ce, configChange{Key: s.Key, Unset: true})
}

// validateSettingValue checks value as the Worker would when it starts, so a
// bad value is refused now and never saved. The error does not repeat value,
// because a mistyped one may be a credential. It returns the value to save:
// a tmux socket path is made absolute, because the Worker may start from
// another working directory (the service runs in the home directory) and must
// find the socket that was validated here.
func validateSettingValue(reg *config.Registry, s config.Setting, value, stateDir string) (string, error) {
	res := &config.Resolver{
		Registry:  reg,
		Role:      config.ScopeWorker,
		Flags:     config.MapSource{s.Key: value, config.KeyStateDir: stateDir},
		LookupEnv: func(string) (string, bool) { return "", false },
	}
	switch s.Key {
	case config.KeyServer, config.KeyHubCAFile, config.KeyHubInsecure:
		_, err := config.ResolveWorkerHub(res)
		return value, err
	case config.KeyTmuxSocket:
		core, err := config.ResolveWorkerCore(res)
		if err != nil {
			return value, err
		}
		if core.TmuxSocket != "" {
			value = core.TmuxSocket
		}
		return value, nil
	case config.KeyName, config.KeyPreviewOnly, config.KeyShareLock:
		_, err := config.ResolveWorkerCore(res)
		return value, err
	}
	_, err := res.Resolve(s.Key)
	return value, err
}

// reportOverride tells the user when a higher source hides the saved value.
func (d workerConfigDeps) reportOverride(ce *configEnv, s config.Setting) error {
	if v, ok := d.lookupEnv(s.EnvName()); ok && v != "" {
		return writef(ce.errOut, "warning: %s is set in the environment and wins over the config file; unset it for this change to take effect.\n", s.EnvName())
	}
	return nil
}

// apply asks a running Worker to apply the change and tells the user what
// will happen. It claims nothing about whether a Worker runs when none was
// asked: the saved value is used at the next start, and a Worker that already
// runs (for example the service) has to be restarted to pick it up.
func (d workerConfigDeps) apply(ctx context.Context, ce *configEnv, c configChange) error {
	switch err := d.applier.ApplyConfig(ctx, ce.stateDir, c); {
	case err == nil:
		return writef(ce.out, "The running Worker applied the change.\n")
	case errors.Is(err, agent.ErrNoLiveWorker):
		return writef(ce.out, "The saved value is used the next time the Worker starts. "+
			"A Worker that is already running (for example the service) must be restarted to pick it up.\n")
	default:
		return writef(ce.out, "The change is saved, but the running Worker could not apply it (%v); restart the Worker to apply it.\n", err)
	}
}

func runConfigShare(cmd *cobra.Command, deps workerConfigDeps, modeArg string, names []string) error {
	mode := store.ShareMode(modeArg)
	if !mode.Valid() {
		return fmt.Errorf("unknown share mode %q; use all, none, select or later", modeArg)
	}
	if mode != store.ShareSelect && len(names) > 0 {
		return fmt.Errorf("session names only go with `select`; %q takes none", modeArg)
	}
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return errors.New("a session name is empty")
		}
	}
	ce, err := deps.env(cmd)
	if err != nil {
		return err
	}
	defer ce.release()
	ctx := cmd.Context()
	st, err := deps.openStore(ctx, ce.stateDir)
	if err != nil {
		return fmt.Errorf("open the Worker state in %s: %w", ce.stateDir, err)
	}
	defer func() { _ = st.Close() }()

	sel, err := st.LoadShareContext(ctx)
	if err != nil {
		return err
	}
	if mode == store.ShareSelect {
		if len(names) > 0 {
			sel.Sessions = names
		}
		if len(sel.Sessions) == 0 {
			return errors.New("name the sessions to share, for example: minion worker config share select build docs")
		}
	}
	sel.Mode = mode

	// The config file may hold share_existing, which the Worker applies to the
	// store at every start. Keep it in step, or the next start would undo this
	// choice. share_existing has no "select", so that mode removes it and the
	// saved selection stands.
	//
	// The file is written first and the store second. A crash between the two
	// leaves the file at the new choice, which the next start applies to the
	// store, so the start never reinstates the old, possibly wider, choice. A
	// store that refuses the write gets the file put back, so the command
	// fails without changing anything.
	prev, had, _ := ce.file.Lookup(config.KeyShareExisting)
	if had {
		if mode == store.ShareSelect {
			ce.file.Delete(config.KeyShareExisting)
		} else {
			ce.file.Set(config.KeyShareExisting, string(mode))
		}
		if err := ce.file.Save(); err != nil {
			return fmt.Errorf("the config file %s could not be updated, nothing was changed: %w", ce.file.Path(), err)
		}
	}
	if err := st.SaveShareContext(ctx, sel); err != nil {
		if had {
			ce.file.Set(config.KeyShareExisting, prev)
			if rerr := ce.file.Save(); rerr != nil {
				return fmt.Errorf("%w; the config file %s could not be put back (%v), so the Worker applies the new share_existing at its next start", err, ce.file.Path(), rerr)
			}
		}
		return err
	}

	if err := writef(ce.out, "Share mode is now %s.\n", describeShare(sel)); err != nil {
		return err
	}
	if s, ok := deps.reg().Lookup(config.KeyShareExisting); ok {
		if err := deps.reportOverride(ce, s); err != nil {
			return err
		}
	}
	return writef(ce.out, "A running Worker reads the share choice from its state database, so the change takes effect at once.\n")
}

func describeShare(sel store.ShareSelection) string {
	if sel.Mode == store.ShareSelect {
		return "select (" + strings.Join(sel.Sessions, ", ") + ")"
	}
	return string(sel.Mode)
}
