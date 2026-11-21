# AI agents: the Minion plugin, `minion llm-help` and the Minion master

Back to the [README](../../README.md).

Claude Code, Codex and other agents can operate Minion the way a person does: check whether Minion is installed
here, install it, start and pair a Worker, read and type into sessions, and, when an administrator allows it,
control sessions on other machines as the **Minion master**.

## The guide for agents: `minion llm-help`

The first line of `minion --help` tells an agent to run `minion llm-help`. That command prints the operating
guide: the rules for agents (ask before installing, sharing or typing; read before writing; outcomes and exit
codes), how to detect what is installed, install, pair, use sessions, use the MCP tools, and become a Minion
master. The plugin's skill is the same text (one source, `internal/llmguide/guide.md`; a test keeps them equal).

## The plugin for Claude Code and Codex

This repository is a plugin marketplace for both tools. The plugin `minion` has the skill above and, in Claude
Code, the command `/minion:setup`, which detects an installed minion and a paired Worker and, only when the user
agrees, installs minion with the one-line installer, starts a Worker and walks through pairing.

Claude Code:

```text
/plugin marketplace add patrick-1984/ai-minion
/plugin install minion@minion
```

Codex:

```sh
codex plugin marketplace add patrick-1984/ai-minion
codex plugin add minion@minion
```

To offer it in your own marketplace, point an entry at this repository's `plugins/minion` folder (Claude Code:
`.claude-plugin/marketplace.json`; Codex: `.agents/plugins/marketplace.json`).

If minion is already installed and the Worker is paired, the plugin changes nothing: the skill uses the existing
`minion` binary, Worker and Hub. For MCP tools, register `minion mcp` as in
[API keys and MCP](api-keys-and-mcp.md#mcp-minion-as-tools-for-an-agent).

## The Minion master

A **Minion master** is an agent in a **master session** nominated by an administrator in **Access > Minion control**.
Every other session is a **slave session**, the default: it controls
nothing beyond its own Worker. A master session reaches a session on another Worker only when that Worker is
**minion-managed** and **accepts control from** the master's Worker (or from All Workers), the session is shared,
and the master's owner may reach that Worker. Both sides consent: the master's Worker nominates, the target Worker
accepts.

- **Nothing is a master by default**, no Worker accepts control by default, and a Worker shares no session until
  its owner chooses. An agent cannot make itself a master.
- **Existing name-only grants** use `minion workers_list`, then `--target <worker-id>:<session>` on the session
  commands, or the same through `minion mcp --allow-writes`. **Check access** on the Minion control page
  explains why a session can or cannot be reached. [Minion control](minion-control.md)

### Prepare one Claude Code terminal

The session attachment is available with native Claude Code 2.1.330 on Linux tmux. It needs a kernel with
`SO_PEERPIDFD`, an updated Worker, and an approved Hub that supports session attachments. Upstream Linux added
this socket option in 6.5. The Worker probes for the option, so a backport can also satisfy the requirement.
The option pins the actual Unix socket peer even if its numeric process ID is later reused. Native qualification
ran on Linux 7.0.0-38 with tmux 3.6. [Linux process descriptors](https://lpc.events/event/18/contributions/1691/)

1. Share the tmux session with the Hub. In a shell that is the pane's process, deliberately start Claude with
   `exec minion master claude`. To pass Claude options, use `exec minion master claude -- <options>`.
2. In **Access > Minion control**, nominate the prepared terminal session. Target Workers must still accept its
   Worker and share the sessions you want to reach.
3. Use the available master tools to list permitted Workers and sessions, inspect a target and send an ordinary
   prompt to an idle agent. Check the reported outcome before deciding whether to retry a write.

The launcher passes an additive MCP configuration to that Claude invocation. An already running unprepared
Claude session needs a future deliberate launch for attachment. The prepared invocation may show a connected
Minion server with no tools while inactive. A healthy refresh empties that live catalog after revoke. Earlier
tool declarations and results can remain in Claude's conversation or model request. Later calls still need
current Hub authority.

Renaming, moving or replacing the selected terminal or its process ends the attachment. A Worker restart requires
a fresh launch and nomination. Clearing the Claude conversation can continue in the same attached terminal.

```text
                 Hub (grants, sharing, approval: decided by people)
                  ▲                 │
     master  ─────┘                 └────►  Worker B: build   (shared, in the grant)
     Worker A: boss                 └────►  Worker C: tests   (shared, in the grant)
     (session-control grant)               Worker C: private  (not shared: unreachable)
```
