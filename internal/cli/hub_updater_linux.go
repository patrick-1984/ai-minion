//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/hub/selfupdate/truenas"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// hubUpdaterReleasesAPI is the fixed release source of the host executor.
const hubUpdaterReleasesAPI = "https://api.github.com"

// hubUpdaterDeps are the collaborators that tests replace.
type hubUpdaterDeps struct {
	euid     func() int
	ownerUID int
	build    func(cfg truenas.Config, j *truenas.Journal, log *slog.Logger) *truenas.Executor
}

func defaultHubUpdaterDeps() hubUpdaterDeps {
	return hubUpdaterDeps{euid: os.Geteuid, ownerUID: 0, build: buildHubUpdater}
}

func buildHubUpdater(cfg truenas.Config, j *truenas.Journal, log *slog.Logger) *truenas.Executor {
	run := truenas.ExecRunner{}
	cache := filepath.Join(cfg.StateDir, "cache")
	return &truenas.Executor{
		Cfg: cfg, Journal: j, Log: log, Timeouts: truenas.DefaultTimeouts(),
		MW:     &truenas.MidcltClient{Runner: run},
		Docker: truenas.DockerCLI{Runner: run},
		ZFS:    truenas.ZFSCLI{Runner: run},
		Probe:  truenas.HTTPProber{},
		Releases: truenas.VersionReleases{NewUpdater: func() (*version.Updater, error) {
			src, err := version.NewGitHubSource(hubUpdaterReleasesAPI, "minion-hub-updater/"+version.Version)
			if err != nil {
				return nil, err
			}
			return &version.Updater{Source: src,
				Verifier: version.SigstoreVerifierFactory(version.TUFTrustedRoot{Dir: cache}),
				Cache:    version.Cache{Dir: cache}}, nil
		}},
	}
}

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(newHubUpdaterCmd(defaultHubUpdaterDeps()))
	})
}

// tickFailure turns a tick that needs a person into a failing exit with a
// one-line, sanitized message, so the TrueNAS Cron Task reports it.
func tickFailure(res truenas.TickResult) error {
	if res.Tick == truenas.TickRecoveryRequired {
		return fmt.Errorf("hub-updater: recovery required (%s): the Hub may be stopped; see docs/install/hub-truenas-updater.md#recovery", res.Detail)
	}
	return nil
}

func newHubUpdaterCmd(deps hubUpdaterDeps) *cobra.Command {
	var (
		cfgPath                          string
		cfg                              truenas.Config
		enabled                          bool
		clearQuarantine, resolveRecovery bool
		timeout                          time.Duration
	)
	cmd := &cobra.Command{
		Use:   "hub-updater",
		Short: "Update the Hub of a TrueNAS custom app (run as root on the NAS host, from a Cron Task)",
		Long: `Runs one tick of the Hub updater on a TrueNAS host. Register it as a TrueNAS
Cron Task that runs as root every 15 minutes:

  /mnt/tank/minion-hub-updater/minion hub-updater --config /mnt/tank/minion-hub-updater/config.json

Every tick first finishes or undoes an unfinished update (also outside the
update window and when the Hub is down). Inside the window, and only when
enabled, it selects the newest verified release of the channel, snapshots the
Hub data dataset while the app is stopped, switches the app to the signed
image digest, waits for sustained readiness and restores the snapshot and the
previous configuration if that fails. See docs/install/hub-truenas-updater.md.

The config file is JSON, owned by root and not writable by others; a flag
overrides the same setting of the file. Keep the binary, the config and the
state directory on a persistent dataset (not /usr, /etc or /var/lib).
The command exits non-zero when an update needs recovery by hand.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deps.euid() != 0 {
				return errors.New("hub-updater changes TrueNAS apps and ZFS datasets: run it as root")
			}
			base := truenas.Config{}
			if path := cfgPath; path != "" {
				c, err := truenas.LoadConfig(path, deps.ownerUID)
				if err != nil {
					return err
				}
				base = c
			}
			f := cmd.Flags()
			set := func(name string, dst *string, v string) {
				if f.Changed(name) {
					*dst = v
				}
			}
			set("app", &base.App, cfg.App)
			set("service", &base.Service, cfg.Service)
			set("dataset", &base.Dataset, cfg.Dataset)
			set("data-mount", &base.DataMount, cfg.DataMount)
			set("window", &base.Window, cfg.Window)
			set("channel", &base.Channel, cfg.Channel)
			set("state-dir", &base.StateDir, cfg.StateDir)
			set("request-file", &base.RequestFile, cfg.RequestFile)
			set("status-file", &base.StatusFile, cfg.StatusFile)
			if f.Changed("enabled") {
				base.Enabled = enabled
			}
			base = base.WithDefaults()
			if err := base.Validate(); err != nil {
				return err
			}
			j, err := truenas.OpenJournal(base.StateDir, base.App, deps.ownerUID, nil)
			if err != nil {
				return err
			}
			defer func() { _ = j.Close() }()
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			ex := deps.build(base, j, log)
			switch {
			case clearQuarantine:
				return ex.ClearQuarantine()
			case resolveRecovery:
				return ex.ResolveRecovery()
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			res, err := ex.Tick(ctx)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "hub-updater: %s %s\n", res.Tick, res.Detail)
			if err != nil {
				return err
			}
			return tickFailure(res)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfgPath, "config", "", "config file (root-owned JSON on a persistent dataset); without it every setting comes from flags")
	f.StringVar(&cfg.App, "app", "", "TrueNAS app name")
	f.StringVar(&cfg.Service, "service", "", "compose service of the Hub (default minion-hub)")
	f.StringVar(&cfg.Dataset, "dataset", "", "ZFS dataset that holds exactly the Hub data, like pool/apps/hub-data")
	f.StringVar(&cfg.DataMount, "data-mount", "", "Hub data path inside the container (default /data)")
	f.StringVar(&cfg.Window, "window", "", "UTC update window HH-HH (default 03-05)")
	f.StringVar(&cfg.Channel, "channel", "", "release channel: latest, X or X.Y (default latest)")
	f.StringVar(&cfg.StateDir, "state-dir", "", "root-owned journal directory on a persistent dataset, outside the Hub data (required)")
	f.StringVar(&cfg.RequestFile, "request-file", "", "request file the Hub (or the owner) writes")
	f.StringVar(&cfg.StatusFile, "status-file", "", "status file the executor writes for the Hub")
	f.BoolVar(&enabled, "enabled", false, "allow unattended updates inside the window")
	f.BoolVar(&clearQuarantine, "clear-quarantine", false, "forget the quarantined (failed) digests and exit")
	f.BoolVar(&resolveRecovery, "resolve-recovery", false, "close an attempt that needs recovery, after restoring the app by hand, and exit")
	f.DurationVar(&timeout, "timeout", 45*time.Minute, "upper bound of one run")
	return cmd
}
