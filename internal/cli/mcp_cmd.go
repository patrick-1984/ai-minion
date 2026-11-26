package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/claudeallow"
	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/mcp"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
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
	// allowlist is the path of the allowlist of "minion claude allow"; ""
	// is claudeallow.DefaultPath (a test seam).
	allowlist string
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
pipe_send) appear only with --allow-writes or MINION_MCP_ALLOW_WRITES=true,
or when the owner approved a standing allow rule with write ops ("minion
claude allow"). While such an allowlist exists, a write (or a read tool that a
rule names) to a Worker or session outside every rule is refused before it is
sent; the allowlist is read on every such call, so "minion claude revoke"
applies at once, and write tools that exist only for those rules refuse every
write once the last rule is revoked. Register the server
under the name "minion" so that its Claude Code tools are mcp__minion__<op>.
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
	stderr := deps.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	logger, err := commandLogger(cmd, res, stderr)
	if err != nil {
		return err
	}
	backend, trust, err := selectMCPBackend(cmd, deps, logger)
	if err != nil {
		return err
	}
	gate, err := mcpGate(cmd, deps, backend)
	if err != nil {
		return err
	}
	allow, gate = mcpWrites(allow, gate)
	if gate.Required {
		logger.Info("minion mcp exposes write tools for the standing allow rules of minion claude allow")
	}
	srv, err := mcp.New(mcp.Config{
		Backend:     backend,
		AllowWrites: allow,
		Gate:        gate,
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

// mcpWrites decides whether the write tools exist: with --allow-writes, or
// when the owner approved standing rules with write ops ("minion claude
// allow"); the gate limits the latter to the targets of those rules. Write
// tools that exist only for those rules make the gate Required: once the last
// rule is revoked, it refuses every write.
func mcpWrites(allow bool, gate claudeallow.Gate) (bool, claudeallow.Gate) {
	if !allow && gate.Active() {
		gate.Required = true
		return true, gate
	}
	return allow, gate
}

// mcpGate returns the gate of the standing allow rules: the allowlist of
// "minion claude allow", read on every call. A target that names no Worker is
// judged as backend runs it: with a Hub, on the --hub-worker of the server
// (the Hub backend names it in the request); on the local control socket, on
// the local Worker, whatever --hub-worker says, so the gate asks that Worker
// for its id.
func mcpGate(cmd *cobra.Command, deps mcpDeps, backend client.Backend) (claudeallow.Gate, error) {
	path := deps.allowlist
	if path == "" {
		p, err := claudeallow.DefaultPath()
		if err != nil {
			return claudeallow.Gate{}, err
		}
		path = p
	}
	cfg, err := resolveHubSettings(cmd.Flags(), deps.lookupEnv, deps.loadConfig)
	if err != nil {
		return claudeallow.Gate{}, fmt.Errorf("configure the Hub backend: %w", err)
	}
	if cfg.Configured() {
		return claudeallow.Gate{Path: path, DefaultWorker: cfg.Worker}, nil
	}
	return claudeallow.Gate{Path: path, LocalWorker: localWorkerID(backend)}, nil
}

// localWorkerID returns a function that asks the local Worker behind b for its
// id (the id its Hub gave it), once: the first answer with an id is kept for
// the life of the server; a failure is asked again at the next call.
func localWorkerID(b client.Backend) func() (string, error) {
	var (
		mu sync.Mutex
		id string
	)
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if id != "" {
			return id, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		raw, err := b.Call(ctx, agent.OpLocalStatus, nil)
		if err != nil {
			return "", err
		}
		var reply agent.LiveStatusReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			return "", err
		}
		if !ops.ValidUUIDv7(reply.WorkerID) {
			return "", errors.New("the local Worker has no Worker id")
		}
		id = reply.WorkerID
		return id, nil
	}
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
		// As for the op commands: sessions_list names --hub-worker as its
		// worker_id, so that the local Worker forwards it (session control).
		localBackend = localHubWorker(local(), cfg.Worker)
	}
	b, kind, err := client.Select(hubConfig(cfg), localBackend,
		client.WithHubUserAgent("minion/"+version.Get().Version),
		client.WithHubWarn(func(msg string) { logger.Warn(msg) }),
		client.WithHubNotice(func(msg string) { logger.Info(msg) }))
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
