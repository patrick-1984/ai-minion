package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// versionFlag is the flag that installs one exact version. It is not a setting:
// the saved and environment form of a version selector is the "channel"
// setting.
const versionFlag = "version"

// updateTimeout bounds one "minion update" run, downloads included.
const updateTimeout = 20 * time.Minute

// HubRangeFunc reports the protocol range the Hub that the local Worker is
// connected to supports (the proto_min..proto_max of its welcome frame,
// Story 4.3). connected is false when there is no Hub connection, in which
// case the update has no range to check.
type HubRangeFunc func(ctx context.Context) (rng version.ProtoRange, connected bool, err error)

var (
	hubRangeMu   sync.Mutex
	hubRangeFunc HubRangeFunc
)

// SetHubRange installs the source of the connected Hub's protocol range. The
// Worker's local API client calls it from its own file once it exists.
func SetHubRange(fn HubRangeFunc) {
	hubRangeMu.Lock()
	defer hubRangeMu.Unlock()
	hubRangeFunc = fn
}

func currentHubRange() HubRangeFunc {
	hubRangeMu.Lock()
	defer hubRangeMu.Unlock()
	return hubRangeFunc
}

// updateDeps are the collaborators of the update command. Tests replace them.
type updateDeps struct {
	// newUpdater builds the updater for the resolved settings.
	newUpdater func(apiURL, cacheDir string, out io.Writer) (*version.Updater, error)
	// lookupEnv reads the environment.
	lookupEnv func(string) (string, bool)
	// current is the running version.
	current func() string
	// execPath is the binary to replace; empty means the running binary.
	execPath string
	// loadConfig reads the Worker config file under the resolved state
	// directory. Nil means no config file is consulted.
	loadConfig func(stateDir string) (config.FileSource, error)
}

func defaultUpdateDeps() updateDeps {
	return updateDeps{
		newUpdater: func(apiURL, cacheDir string, out io.Writer) (*version.Updater, error) {
			src, err := version.NewGitHubSource(apiURL, "minion/"+version.Version)
			if err != nil {
				return nil, err
			}
			return &version.Updater{
				Source:   src,
				Verifier: version.SigstoreVerifierFactory(version.TUFTrustedRoot{Dir: cacheDir}),
				Cache:    version.Cache{Dir: cacheDir},
				Out:      out,
			}, nil
		},
		lookupEnv:  os.LookupEnv,
		current:    func() string { return version.Version },
		loadConfig: loadWorkerConfig,
	}
}

// workerConfigName is the file name of the Worker config file inside the
// state directory (AD-16).
const workerConfigName = "config.json"

// loadWorkerConfig reads the Worker config file of the state directory. A
// missing file is empty; an unreadable or too open one is an error, because
// a saved channel pin must never be ignored silently.
func loadWorkerConfig(stateDir string) (config.FileSource, error) {
	return config.LoadFile(filepath.Join(stateDir, workerConfigName))
}

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(newUpdateCmd(defaultUpdateDeps()))
	})
}

// changedFlags is a [config.FlagSource] over the flags the user set.
type changedFlags struct {
	cmd *cobra.Command
	reg *config.Registry
}

func (c changedFlags) Lookup(key string) (string, bool, error) {
	s, ok := c.reg.Lookup(key)
	if !ok || !s.HasFlag() {
		return "", false, nil
	}
	f := c.cmd.Flags().Lookup(s.FlagName())
	if f == nil || !f.Changed {
		return "", false, nil
	}
	return f.Value.String(), true, nil
}

func newUpdateCmd(deps updateDeps) *cobra.Command {
	var (
		allowDowngrade bool
		offline        bool
		hubProto       string
		exactVersion   string
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update minion to an exact version or a channel, with verified signatures",
		Long: `Update replaces this binary with a signed release.

The selector works like a Docker tag: vX.Y.Z is one exact version, X.Y and X
follow the newest matching release, latest follows the newest release. A
release candidate (-rc) never satisfies a channel.

--version vX.Y.Z (or vX.Y.Z-rc.N) installs exactly that version, which is how
a release candidate is installed. It takes no channel; use --channel for X.Y,
X and latest. --version and --channel cannot be combined.

Before anything is installed, the signature of the release is verified: the
certificate identity must be the release workflow of this project for the exact
tag, the issuer GitHub Actions, and the signature must be in the Rekor
transparency log with an inclusion proof. Then the archive checksum is verified
and the binary is replaced atomically.

An online update contacts two hosts: the release host (GitHub) and the public
Sigstore TUF repository (tuf-repo-cdn.sigstore.dev), from which the Sigstore
trusted root is fetched. With --offline nothing is contacted. Nothing else in
minion contacts either host.

An older version is refused unless --allow-downgrade is given. When this
machine's Worker is connected to a Hub, an update whose protocol range does not
overlap the Hub's is refused. With --offline the newest verified version in the
local cache that matches the selector is installed.`,
		Args: cobra.NoArgs,
	}
	cmd.Flags().String(settingFlag(config.KeyChannel), "", "version selector: vX.Y.Z, X.Y, X or latest (env MINION_CHANNEL, default latest)")
	cmd.Flags().StringVar(&exactVersion, versionFlag, "", "install exactly this version, vX.Y.Z or vX.Y.Z-rc.N (not a channel, not with --channel)")
	cmd.Flags().String(settingFlag(config.KeyUpdateAPIURL), "", "GitHub API root to list releases from (env MINION_UPDATE_API_URL)")
	cmd.Flags().String(settingFlag(config.KeyUpdateCacheDir), "", "directory for verified releases (env MINION_UPDATE_CACHE_DIR)")
	cmd.Flags().BoolVar(&allowDowngrade, "allow-downgrade", false, "allow installing a version older than the running one")
	cmd.Flags().BoolVar(&offline, "offline", false, "do not use the network: install the newest verified cached version that matches")
	cmd.Flags().StringVar(&hubProto, "hub-proto-range", "", "protocol range of the connected Hub as MIN..MAX, for example 1.0..1.1 (normally detected from the running Worker)")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		// Every refusal of the input happens before anything is contacted.
		channelFlag := settingFlag(config.KeyChannel)
		if cmd.Flags().Changed(versionFlag) && cmd.Flags().Changed(channelFlag) {
			return fmt.Errorf("--%s and --%s cannot be combined: --%s installs one exact version, --%s follows a channel",
				versionFlag, channelFlag, versionFlag, channelFlag)
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
		}
		get := func(key string) (string, error) {
			r, err := res.Resolve(key)
			return r.Value, err
		}
		var sel version.Selector
		if cmd.Flags().Changed(versionFlag) {
			var err error
			if sel, err = parseExactVersion(exactVersion); err != nil {
				return err
			}
		} else {
			channel, err := get(config.KeyChannel)
			if err != nil {
				return err
			}
			if sel, err = version.ParseSelector(channel); err != nil {
				return err
			}
		}
		apiURL, err := get(config.KeyUpdateAPIURL)
		if err != nil {
			return err
		}
		cacheDir, err := get(config.KeyUpdateCacheDir)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), updateTimeout)
		defer cancel()
		hub, err := hubRange(ctx, hubProto)
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		u, err := deps.newUpdater(apiURL, cacheDir, out)
		if err != nil {
			return err
		}
		result, err := u.Run(ctx, version.Options{
			Selector:       sel,
			Current:        deps.current(),
			AllowDowngrade: allowDowngrade,
			Offline:        offline,
			HubRange:       hub,
			ExecPath:       deps.execPath,
		})
		if err != nil {
			return err
		}
		if result.UpToDate {
			_, err = fmt.Fprintf(out, "minion is up to date (%s)\n", result.Tag)
			return err
		}
		if _, err = fmt.Fprintf(out, "updated minion from %s to %s (%s)\n", result.Previous, result.Tag, result.Path); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "restart running services to use the new version")
		return err
	}
	return cmd
}

// settingFlag returns the flag name of a declared setting.
func settingFlag(key string) string {
	s, ok := config.Default.Lookup(key)
	if !ok {
		panic("cli: setting " + key + " is not declared")
	}
	return s.FlagName()
}

// hubRange returns the Hub range from the flag, else from the registered
// source. Nil means there is no Hub connection to check against.
func hubRange(ctx context.Context, flagValue string) (*version.ProtoRange, error) {
	if flagValue != "" {
		r, err := version.ParseProtoRange(flagValue)
		if err != nil {
			return nil, fmt.Errorf("--hub-proto-range: %w", err)
		}
		return &r, nil
	}
	fn := currentHubRange()
	if fn == nil {
		return nil, nil
	}
	r, connected, err := fn(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("cannot read the Hub's protocol range from the running Worker: %w", err)
	}
	if !connected {
		return nil, nil
	}
	return &r, nil
}

// parseExactVersion parses the value of --version: one exact release, vX.Y.Z
// or vX.Y.Z-rc.N. A channel form is refused with a pointer to --channel.
func parseExactVersion(v string) (version.Selector, error) {
	sel, err := version.ParseSelector(v)
	if err != nil {
		return version.Selector{}, fmt.Errorf("--%s %q: want one exact version, vX.Y.Z or vX.Y.Z-rc.N", versionFlag, v)
	}
	if sel.IsChannel() {
		return version.Selector{}, fmt.Errorf("--%s takes one exact version (vX.Y.Z or vX.Y.Z-rc.N), but %q is a channel; use --%s %s",
			versionFlag, v, settingFlag(config.KeyChannel), v)
	}
	return sel, nil
}

// loadConfigFile reads the Worker config file under the state directory that
// the flags and environment (or the default) name, so that saved settings
// take part in the precedence chain: flag, environment, config file, default.
// A state directory that cannot be determined (no home directory) means there
// is no file to read; the settings then come from the other sources.
func loadConfigFile(res *config.Resolver, load func(stateDir string) (config.FileSource, error)) (config.FileSource, error) {
	dir, err := res.Resolve(config.KeyStateDir)
	if err != nil || dir.Value == "" {
		return nil, nil
	}
	return load(dir.Value)
}
