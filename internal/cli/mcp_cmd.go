package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// allowWritesFlag is the short flag of the mcp_allow_writes setting.
const allowWritesFlag = "allow-writes"

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(newMCPCmd(defaultMCPDeps(os.Stderr)))
	})
}

// mcpDeps are the inputs of "minion mcp" that tests replace.
type mcpDeps struct {
	stderr     io.Writer
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// backend, when set, returns the backend the tools run on and the policy
	// for pipe turns that go to the model, instead of the one the settings
	// choose (a test seam). Nil is the normal case: a configured Hub, else the
	// local Worker (see selectMCPBackend).
	backend func() (client.Backend, mcp.Trust)
	// local returns the local backend used when no Hub is configured.
	local func() client.Backend
}

func defaultMCPDeps(stderr io.Writer) mcpDeps {
	return mcpDeps{
		stderr:     stderr,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		local:      func() client.Backend { return client.Local() },
	}
}

func newMCPCmd(deps mcpDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the Minion operations as MCP tools over stdio",
		Long: `Serve the Minion operation catalog as MCP tools on stdin and stdout, for
Claude Code, Codex and other agent harnesses. Register it as a stdio server
whose command is "minion mcp".

The tools run on the local Worker through its control socket, so a Worker must
be running for this user ("minion worker run"); the server starts without one
and every tool call then says that no Worker is running.

With MINION_HUB_URL and MINION_API_KEY_FILE set (or --hub-url) the tools call
that Hub over its REST API with the API key in that file instead, and never the
local Worker: the same tools, and the Hub decides which sessions the key may use.
The key is read only from the file; it has no flag and no plain environment
variable. Use --hub-ca-file for a Hub with a private CA (--hub-insecure skips
the certificate check for this client only and warns). --hub-worker names the
Worker, by its id (UUIDv7, as listed by workers_list; a name is refused), for
the tools whose arguments name none (sessions_list, session_create).

MCP is read-only: only the listing, snapshot, capture and pipe read tools exist.
The write tools (session_create, send_text, send_keys, set_task, answer,
pipe_send) appear only with --allow-writes or MINION_MCP_ALLOW_WRITES=true.
Terminal screens are sanitised by the Worker before they reach the model.

Standard output carries only protocol messages; logs go to standard error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCP(cmd, deps)
		},
	}
	setting, _ := config.Default.Lookup(config.KeyMCPAllowWrites)
	cmd.Flags().Bool(allowWritesFlag, false, "Expose the write tools (env "+setting.EnvName()+")")
	cmd.Flags().Lookup(allowWritesFlag).NoOptDefVal = "true"
	addHubFlags(cmd.Flags())
	return cmd
}

// mcpFlags is the [config.FlagSource] of "minion mcp": --allow-writes feeds
// the mcp_allow_writes setting when the user set it.
type mcpFlags struct{ cmd *cobra.Command }

func (f mcpFlags) Lookup(key string) (string, bool, error) {
	if key != config.KeyMCPAllowWrites {
		return "", false, nil
	}
	fl := f.cmd.Flags().Lookup(allowWritesFlag)
	if fl == nil || !fl.Changed {
		return "", false, nil
	}
	return fl.Value.String(), true, nil
}

func runMCP(cmd *cobra.Command, deps mcpDeps) error {
	res := &config.Resolver{
		Role:      config.ScopeWorker,
		Flags:     mcpFlags{cmd: cmd},
		LookupEnv: deps.lookupEnv,
	}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(res, deps.loadConfig)
		if err != nil {
			return err
		}
		res.File = file
	}
	allow, err := config.ResolveMCPAllowWrites(res)
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
	stderr := deps.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	logger, err := logx.New(logx.Options{Level: level.Value, Format: logx.Format(format.Value), Output: stderr})
	if err != nil {
		return err
	}
	backend, trust, err := selectMCPBackend(cmd, deps, logger)
	if err != nil {
		return err
	}
	srv, err := mcp.New(mcp.Config{
		Backend:     backend,
		AllowWrites: allow,
		Trust:       trust,
		Version:     version.Get().Version,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	if allow {
		logger.Warn("minion mcp exposes write tools: the model can type into sessions")
	}
	if err := srv.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
		return fmt.Errorf("serve MCP: %w", err)
	}
	return nil
}

// selectMCPBackend chooses the backend of the tools: a configured Hub (MINION_HUB_URL, MINION_API_KEY_FILE, --hub-url and the
// other --hub-* flags), else the backend of the test seam when one is set, else
// the local control socket. A Hub that is configured
// always wins and its failure ends the command: the tools never run on the local
// Worker instead.
//
// The pipe policy of a Hub is the Hub's own: it withholds the turns of peers the
// key's user has not trusted and says how many it withheld, and that notice
// reaches the model unchanged. What is left is trusted. In Solo the OS user owns
// every session, so every peer is trusted (AD-22).
func selectMCPBackend(cmd *cobra.Command, deps mcpDeps, logger *slog.Logger) (client.Backend, mcp.Trust, error) {
	cfg, err := resolveHubSettings(cmd.Flags(), deps.lookupEnv, deps.loadConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("configure the Hub backend: %w", err)
	}
	local := deps.local
	if local == nil {
		local = func() client.Backend { return client.Local() }
	}
	var localBackend client.Backend
	if deps.backend != nil && !cfg.Configured() {
		b, t := deps.backend()
		return b, t, nil
	}
	if !cfg.Configured() {
		localBackend = local()
	}
	hc := client.HubConfig{URL: cfg.URL, APIKeyFile: cfg.APIKeyFile, CAFile: cfg.CAFile, Insecure: cfg.Insecure, Worker: cfg.Worker}
	b, kind, err := client.Select(hc, localBackend,
		client.WithHubUserAgent("minion/"+version.Get().Version),
		client.WithHubWarn(func(msg string) { logger.Warn(msg) }))
	if err != nil {
		return nil, nil, fmt.Errorf("configure the Hub backend: %w", err)
	}
	if kind == client.KindHub {
		// Only the kind of the backend: the address of the Hub is a value a
		// user typed and may hold a credential, so it is not logged at all.
		logger.Info("minion mcp calls a Hub over its REST API")
	}
	return b, mcp.TrustAll, nil
}
