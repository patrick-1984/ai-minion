package cli

import (
	"context"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/server"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

func init() {
	Register(func(root *cobra.Command) {
		hub := hubCommand(root)
		hub.AddCommand(&cobra.Command{
			Use:   "serve",
			Short: "Run the Minion Hub (HTTPS on :8443 by default)",
			Long: `Run the Minion Hub: the HTTP API, the web UI and the Worker channel.

The Hub reads its settings from the environment (it has no flags):

  MINION_DATA_DIR         data directory (database, keys, certificate); must be writable
  MINION_LISTEN           listen address, default :8443
  MINION_TLS              auto (default) or off
  MINION_TLS_CERT_FILE    certificate to use instead of the generated self-signed one
  MINION_TLS_KEY_FILE     private key for MINION_TLS_CERT_FILE (both or neither)
  MINION_PUBLIC_URL       external URL; required when MINION_TLS=off
  MINION_TRUSTED_PROXIES  CIDRs of reverse proxies whose X-Forwarded-* headers are trusted

Without a certificate the Hub generates a self-signed ECDSA P-256 certificate
in <data dir>/tls on first start and logs its SHA-256 fingerprint. Send SIGHUP
to reload a configured certificate.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runHubServe(cmd.Context(), hubServeDeps{stderr: cmd.ErrOrStderr()})
			},
		})
	})
}

// hubCommand returns the "hub" command group, creating it when no other file
// has registered it yet. Several stories add subcommands to it.
func hubCommand(root *cobra.Command) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == "hub" {
			return c
		}
	}
	hub := &cobra.Command{
		Use:   "hub",
		Short: "Run and manage the Minion Hub",
	}
	root.AddCommand(hub)
	return hub
}

// hubServeDeps are the inputs of runHubServe that tests replace.
type hubServeDeps struct {
	stderr    io.Writer
	lookupEnv func(string) (string, bool)
	registry  *server.Registry
	options   []server.Option
	// started, when not nil, receives the listen address once the Hub accepts
	// connections.
	started func(addr string)
}

// runHubServe resolves the Hub settings from the environment and runs the Hub
// until ctx is cancelled.
func runHubServe(ctx context.Context, d hubServeDeps) error {
	res := &config.Resolver{Role: config.ScopeHub, LookupEnv: d.lookupEnv}
	hs, err := config.ResolveHubServer(res)
	if err != nil {
		return err
	}

	level, err := res.Resolve(config.KeyLogLevel)
	if err != nil {
		return err
	}
	format, err := res.Resolve(config.KeyLogFormat)
	if err != nil {
		return err
	}
	logger, err := logx.New(logx.Options{Level: level.Value, Format: logx.Format(format.Value), Output: d.stderr})
	if err != nil {
		return err
	}

	cfg, err := server.NewConfig(hs, logger)
	if err != nil {
		return err
	}
	reg := d.registry
	if reg == nil {
		reg = server.DefaultRegistry
	}
	srv := server.New(cfg, reg, d.options...)
	if d.started != nil {
		go func() {
			select {
			case <-srv.Started():
				d.started(srv.Addr())
			case <-ctx.Done():
			}
		}()
	}
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
