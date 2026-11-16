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

A **Minion master** is one agent session that controls sessions on other Workers through the Hub: it lists the
Workers it may reach, reads their screens and types into their sessions. It is an ordinary tmux session on a
paired Worker that an administrator gave a **session-control grant** (Administration > Session control).

- **Nothing is a master by default.** A newly connected Worker and its sessions control nothing beyond their own
  machine, and a Worker shares no session until its owner chooses. An agent cannot make itself a master: the
  grant, the approval of the Worker and the sharing are made by people in the Hub.
- **What a master can reach** is the grant's targets (All, or chosen Workers and sessions), never more than the
  owner of the master's Worker may reach, and only sessions their Workers share.
- **How to use it:** inside the master session, `minion workers_list`, then `--target <worker-id>:<session>` on the
  session commands, or the same through `minion mcp --allow-writes`. Details and limits:
  [Session control](session-control.md).

```text
                 Hub (grants, sharing, approval: decided by people)
                  ▲                 │
     master  ─────┘                 └────►  Worker B: build   (shared, in the grant)
     Worker A: boss                 └────►  Worker C: tests   (shared, in the grant)
     (session-control grant)               Worker C: private  (not shared: unreachable)
```
