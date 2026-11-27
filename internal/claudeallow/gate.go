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

// ErrNoRuleLeft is the refusal of a write by a server whose write tools came
// only from standing rules, after the last rule was revoked.
var ErrNoRuleLeft = proto.NewError(proto.CodeForbidden, "no standing Minion allow rule is left, and this minion mcp offers write tools only for such rules; nothing was sent. Ask the owner, or restart minion mcp.")

// ErrCodeTarget is the refusal of a session code as the target of a call
// that a standing rule must allow: the Hub resolves a code to any Worker, so a
// rule cannot be matched against it.
var ErrCodeTarget = proto.NewError(proto.CodeForbidden, "a session code is not a target of a standing Minion allow rule; name the session by worker_id and session_name (worker-id:session). Nothing was sent.")

// Gate enforces the allowlist at Path for "minion mcp".
type Gate struct {
	// Path is the allowlist file ([DefaultPath]).
	Path string
	// DefaultWorker is the Worker the backend sends a target without a
	// worker_id to: the --hub-worker of a Hub backend, which names it in the
	// request. "" when the server has none.
	DefaultWorker string
	// LocalWorker, when set, returns the id of the local Worker, and a target
	// without a worker_id is one of its sessions (the local control socket
	// without a --hub-worker runs it there). It replaces DefaultWorker;
	// an error or an empty id refuses the call (fail closed).
	LocalWorker func() (string, error)
	// Required is set when the write tools exist only because of the
	// allowlist (no --allow-writes): without an allowlist file every write
	// is then refused instead of passing.
	Required bool
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
// Without an allowlist file every op passes with no rule (the server behaves
// as it did before standing rules existed), except a write of a server whose
// write tools came only from the allowlist ([Gate.Required]), which is
// refused. With one, a write passes only when a rule allows op on its target,
// and Check returns that rule's id; otherwise it returns [ErrNotAllowed]. A
// read that some rule names (Claude Code runs it with no prompt) is limited to
// the rules' targets in the same way; any other read passes, and so do
// sessions_list and workers_list, which name no session. A session code
// without a worker_id is refused ([ErrCodeTarget]). An allowlist that cannot be
// read refuses every write and every read with a target (fail closed).
func (g Gate) Check(op ops.Op, args json.RawMessage) (string, error) {
	write := op.ChangesState()
	if !write && (op.Name == ops.OpSessionsList || op.Name == ops.OpWorkersList) {
		return "", nil
	}
	f, err := Load(g.Path)
	if err != nil {
		return "", proto.NewError(proto.CodeForbidden, fmt.Sprintf("the Minion allowlist cannot be read (%s); nothing was sent; ask the owner to run minion claude status", errText(err)))
	}
	if f == nil {
		if write && g.Required {
			return "", ErrNoRuleLeft
		}
		return "", nil
	}
	if !write && !f.Names(op.Name) {
		return "", nil
	}
	worker, session, ok := targetOf(args)
	if !ok {
		return "", ErrNotAllowed
	}
	if worker == "" && session != "" {
		if _, _, isCode := ops.ParseSessionCode(session); isCode {
			return "", ErrCodeTarget
		}
	}
	if worker == "" {
		if worker, err = g.defaultWorker(); err != nil {
			return "", err
		}
	}
	r, ok := f.Match(op.Name, worker, session)
	if !ok {
		return "", ErrNotAllowed
	}
	return r.ID, nil
}

// defaultWorker returns the Worker a target without a worker_id runs on.
func (g Gate) defaultWorker() (string, error) {
	if g.LocalWorker == nil {
		return g.DefaultWorker, nil
	}
	id, err := g.LocalWorker()
	if err != nil || id == "" {
		return "", proto.NewError(proto.CodeForbidden, "the id of the local Worker is not known (is it running and connected to its Hub?), so a target without a worker_id cannot be matched against a standing Minion allow rule; nothing was sent")
	}
	return id, nil
}

// targetOf reads the Worker and session a tool call names: target.worker_id
// and target.session_name, else a top-level worker_id (an op on a Worker).
// ok is false for arguments it cannot read.
func targetOf(args json.RawMessage) (worker, session string, ok bool) {
	var a struct {
		WorkerID string `json:"worker_id"`
		Target   *struct {
			WorkerID    string `json:"worker_id"`
			SessionName string `json:"session_name"`
		} `json:"target"`
	}
	if json.Unmarshal(args, &a) != nil {
		return "", "", false
	}
	if a.Target != nil {
		return a.Target.WorkerID, a.Target.SessionName, true
	}
	return a.WorkerID, "", true
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
