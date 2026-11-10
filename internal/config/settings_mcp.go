package config

import "strconv"

// KeyMCPAllowWrites names the setting that lets `minion mcp` expose the write
// tools (AD-22, Story 2.10).
const KeyMCPAllowWrites = "mcp_allow_writes"

func init() { declareMCP(Default) }

// declareMCP declares the settings of `minion mcp`. MCP is read-only unless
// the owner turns writes on, so a model cannot type into a session by default
// (AD-22). The environment name is MINION_MCP_ALLOW_WRITES; the command also
// has the short flag --allow-writes.
func declareMCP(r *Registry) {
	r.Declare(Setting{
		Key:     KeyMCPAllowWrites,
		Scope:   ScopeWorker,
		Help:    "Let minion mcp expose the write tools (session_create, send_text, send_keys, set_task, answer, pipe_send); without it MCP is read-only (true or false)",
		Default: "false",
		Choices: []string{"true", "false"},
	})
}

// ResolveMCPAllowWrites returns whether `minion mcp` may expose write tools.
// The resolver must be a Worker resolver.
func ResolveMCPAllowWrites(res *Resolver) (bool, error) {
	v, err := res.Resolve(KeyMCPAllowWrites)
	if err != nil {
		return false, err
	}
	b, err := strconv.ParseBool(v.Value)
	if err != nil {
		return false, err
	}
	return b, nil
}
