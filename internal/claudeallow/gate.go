package claudeallow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// NotAllowedMessage is the refusal of a write outside every standing rule.
const NotAllowedMessage = "not in a standing Minion allow rule; ask the owner or use the CLI. Nothing was sent."

// ErrNotAllowed is the refusal of a write outside every standing rule. It
// matches proto.CodeForbidden for the model; nothing was sent.
var ErrNotAllowed = proto.NewError(proto.CodeForbidden, NotAllowedMessage)

// Gate enforces the allowlist at Path for "minion mcp".
type Gate struct {
	// Path is the allowlist file ([DefaultPath]).
	Path string
	// DefaultWorker is the Worker a target without a worker_id goes to (the
	// --hub-worker of the server), or "" when the server has none.
	DefaultWorker string
}

// Active reports whether the allowlist exists and grants a write, so that
// "minion mcp" exposes its write tools. A broken file is not active.
func (g Gate) Active() bool {
	f, err := Load(g.Path)
	return err == nil && f.GrantsWrites()
}

// Check decides the tool call of op with the arguments args. It reads the
// allowlist again on every call.
//
// An op that does not change state passes with no rule. Without an
// allowlist file every op passes with no rule: the server behaves as it did
// before standing rules existed. With one, a write passes only when a rule
// allows op on its target, and Check returns that rule's id; otherwise it
// returns [ErrNotAllowed]. An allowlist that cannot be read refuses every
// write (fail closed).
func (g Gate) Check(op ops.Op, args json.RawMessage) (string, error) {
	if !op.ChangesState() {
		return "", nil
	}
	f, err := Load(g.Path)
	if err != nil {
		return "", proto.NewError(proto.CodeForbidden, fmt.Sprintf("the Minion allowlist cannot be read (%s); nothing was sent; ask the owner to run minion claude status", errText(err)))
	}
	if f == nil {
		return "", nil
	}
	worker, session := targetOf(args)
	if worker == "" {
		worker = g.DefaultWorker
	}
	r, ok := f.Match(op.Name, worker, session)
	if !ok {
		return "", ErrNotAllowed
	}
	return r.ID, nil
}

// targetOf reads the Worker and session a tool call names: target.worker_id
// and target.session_name, else a top-level worker_id (an op on a Worker).
func targetOf(args json.RawMessage) (string, string) {
	var a struct {
		WorkerID string `json:"worker_id"`
		Target   *struct {
			WorkerID    string `json:"worker_id"`
			SessionName string `json:"session_name"`
		} `json:"target"`
	}
	if json.Unmarshal(args, &a) != nil {
		return "", ""
	}
	if a.Target != nil {
		return a.Target.WorkerID, a.Target.SessionName
	}
	return a.WorkerID, ""
}

func errText(err error) string {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return strings.TrimSpace(err.Error())
}

// ToolEntry returns the Claude Code permission rule of the tool op of the MCP
// server named server: mcp__<server>__<op>, with every character of the
// server name outside A-Z, a-z, 0-9, "_" and "-" replaced by "_", as Claude
// Code names it.
func ToolEntry(server, op string) string {
	return "mcp__" + sanitize(server) + "__" + op
}

// PluginServer returns the server name under which Claude Code serves the
// server of the plugin plugin: plugin_<plugin>_<server>.
func PluginServer(plugin, server string) string {
	return "plugin_" + plugin + "_" + server
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
