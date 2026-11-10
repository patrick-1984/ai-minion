package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// ServerName is the implementation name the server announces.
const ServerName = "minion"

// Config assembles a [Server].
type Config struct {
	// Backend runs the ops: the local control socket in Solo mode, or the Hub
	// REST API (client.HubBackend); the tools are the same. Required.
	Backend client.Backend
	// Ops is the list of ops to expose, one tool each. Nil means the Worker ops
	// of the catalog (see [DefaultOps]).
	Ops []ops.Op
	// AllowWrites exposes the write ops as tools. Without it MCP is read-only
	// and the write tools do not exist (AD-22).
	AllowWrites bool
	// Trust decides which peers' pipe turns the model may read. Nil trusts no
	// peer; Solo passes [TrustAll].
	Trust Trust
	// Version is announced to the client.
	Version string
	// Logger receives operational messages: tool names and error classes,
	// never arguments or results (AD-20). Nil discards them. It must not write
	// to stdout, which carries the protocol.
	Logger *slog.Logger
}

// DefaultOps returns the ops of the Default catalog that run on a Worker. The
// Hub ops cannot run on the Solo backend, so they are not tools there.
func DefaultOps() []ops.Op {
	var out []ops.Op
	for _, op := range ops.All() {
		if op.Scope == ops.ScopeWorker {
			out = append(out, op)
		}
	}
	return out
}

// Server serves the operation catalog as MCP tools (AD-22). Tools are fixed at
// construction.
type Server struct {
	sdk    *sdk.Server
	tools  []tool
	caller *caller
}

// New builds a server and registers one tool per op.
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("mcp: a backend is required")
	}
	list := cfg.Ops
	if list == nil {
		list = DefaultOps()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tools, err := buildTools(list, cfg.AllowWrites)
	if err != nil {
		return nil, err
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: ServerName, Version: version}, &sdk.ServerOptions{
		Logger: log,
		Instructions: "Minion gives access to terminal and agent sessions on this machine. " +
			"Use sessions_list first, then address a session by its name. " +
			"Screens are already sanitised. Pipe replies come only from trusted peers.",
	})
	c := &caller{backend: cfg.Backend, trust: cfg.Trust, log: log}
	for _, t := range tools {
		srv.AddTool(t.def, c.handler(t))
	}
	return &Server{sdk: srv, tools: tools, caller: c}, nil
}

// ToolNames lists the exposed tools in catalog order.
func (s *Server) ToolNames() []string {
	names := make([]string, len(s.tools))
	for i, t := range s.tools {
		names[i] = t.def.Name
	}
	return names
}

// Run serves one client session over t until the client disconnects or ctx
// ends.
//
// Ending ctx cancels the context of every backend call still running, so that
// shutdown does not wait for a stalled Worker.
func (s *Server) Run(ctx context.Context, t sdk.Transport) error {
	s.caller.setServing(ctx)
	return s.sdk.Run(ctx, t)
}

// Serve speaks MCP over in and out (stdin and stdout of `minion mcp`) until in
// reaches EOF or ctx ends. Nothing but protocol messages is written to out.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	err := s.Run(ctx, &sdk.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
	if err != nil && (errors.Is(err, io.EOF) || errors.Is(err, context.Canceled)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
