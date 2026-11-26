# Using Minion from Claude Code without per-command prompts

Back to the [README](../../README.md). Related: [API keys and MCP](api-keys-and-mcp.md), [AI agents](agents.md), [Access](access.md).

A Claude Code session that drives other Minion sessions (a master session, or one that holds a grant) sends many small commands: read a screen, type a prompt, press Enter. In auto mode, Claude Code's classifier often stops these calls and asks you, because typing into another terminal looks like a risky action to it. A **standing allow rule** is your approval, given once, for a fixed list of Minion tools on Workers and sessions you choose. Claude Code then runs those calls without asking. Everything else is still asked.

## What is allowed

One rule holds:

- **Ops**: the Minion MCP tools Claude Code may run without a prompt. The default is the session ops of a master: `send_text`, `send_keys`, `answer`, `session_snapshot`, `session_capture`, `sessions_list`, `workers_list`, `pipe_send`, `pipe_read`. No admin op is ever in the default. You can list other tools of `minion mcp` with `--ops` (for example `set_task`); the command then warns you. Ops that `minion mcp` does not serve, such as `invitation_create`, cannot be allowed: they stay on the normal path, which asks you.
- **Targets**: one or more Workers, by id, and optionally the session names on them. Without `--session` the rule reaches every session of those Workers.

A rule changes two files, both yours and both outside any project:

| File | What Minion writes |
|---|---|
| `~/.claude/settings.json` (Claude Code user settings; the profile of `CLAUDE_CONFIG_DIR` when set) | One `permissions.allow` entry per tool, for example `mcp__minion__send_text`. With the minion-control plugin installed, also `mcp__plugin_minion-control_minion__send_text`, the name under which Claude Code serves the plugin's server. |
| `~/.config/minion/claude-allow.json` (`$XDG_CONFIG_HOME/minion/`) | The rule: its id (a UUIDv7), when you created it, its ops, its targets, and the settings entries it needs. |

Why two files: a Claude Code allow rule for an MCP tool names the tool only, never its arguments, so `mcp__minion__send_text` on its own would allow typing into **any** session. Minion keeps the target limit itself. While the allowlist file exists, `minion mcp` refuses a write to a Worker or session outside every rule, before anything is sent, with the error `not in a standing Minion allow rule; ask the owner or use the CLI`. It reads the file again on every write call.

Explicit allow rules are applied before the auto mode classifier, so a matching call runs without a prompt and without a classifier verdict. A Minion write that the allowlist refuses never reaches the target.

Nothing in Minion's own checks changes: the Hub still decides every call (sharing, grants, master sessions, preview-only Workers, approvals). A rule only saves the Claude Code prompt for calls Minion would allow anyway. Nothing is sent to, or shown in, the sessions you control.

## Grant

1. Register `minion mcp` in Claude Code under the name `minion`. With the minion-control plugin (0.2.0 or later) this is done for you. Without it:

   ```sh
   claude mcp add --scope user minion -- minion mcp
   ```

2. Find the Worker id: `minion workers_list` (or the Workers page of the Hub).
3. In a terminal of your own (not inside Claude Code), run:

   ```sh
   minion claude allow --worker 01890a5d-ac96-774b-bcce-b302099a8057 --session shell
   ```

   It prints the rule id, the targets, the ops, and each settings entry it adds (`+`) or finds already there (`=`), then asks `Add this rule? [y/N]`. Only `y` writes. `--yes` skips the question for scripts; without a terminal and without `--yes` nothing is written. Repeat `--worker` and `--session` for more targets; use `--mcp-server-name` when you registered the server under another name.

4. Start a new Claude Code session (or run `/mcp`) so that `minion mcp` offers its write tools. While an allowlist with write ops exists, `minion mcp` exposes the write tools even without `--allow-writes`, limited to the rules' targets.

`minion claude allow` refuses to run inside a Claude Code session (when `CLAUDECODE` is set): an agent must not grant itself standing permissions.

## See and revoke

```sh
minion claude status          # every rule: id, created, targets, ops, and whether each settings entry is present
minion claude status --json   # the same for scripts and the plugin's session hook
minion claude revoke 019a...  # one rule
minion claude revoke --all    # every rule
```

`status` also lists Minion tool entries in the settings that no rule needs (added by hand). Treat those with care: with no allowlist they are not limited to any target.

`revoke` removes the rule from the allowlist and removes from the settings the entries that it added and no other rule needs; an entry that was in your settings before stays. A copy of the replaced settings is kept in `settings.json.minion-backup`. The revoke applies at once: the next write call of a running `minion mcp` reads the allowlist again, and Claude Code asks again for tools whose entries are gone. When the last rule is revoked, the allowlist file is deleted and `minion mcp` behaves as before.

## Audit

Every write sent under a rule carries the rule id to the Hub. The Hub audit event of the write (Administration > Audit, or `audit_list`) shows it in the detail as `client_rule=<rule id>`, next to the actor, the target, the source session and the grant. The id is a label the client sends: the Hub records it and never decides anything by it. Compare it with `minion claude status` to see which approval a write came from. Writes to a session on the same Worker as `minion mcp`, which do not pass through the Hub, are not in the Hub audit.

## The security trade-off, in plain words

- **You give up a prompt, not a check.** Without a rule, you see and approve each command. With a rule, Claude Code types into the chosen sessions on its own, as often as it decides to, for as long as the rule exists. If the agent is confused or is steered by something it read (a web page, a file, another session's screen), it can send unwanted input to those sessions without asking you. Minion still enforces who may reach what, but within the rule nobody asks.
- **A rule is per user, not per session.** The settings are your Claude Code user settings, so every Claude Code session of your user with the `minion` server gets the same tools without a prompt. The Hub still decides by the calling session: a session on another Worker is reached only from a session that holds a grant or a master nomination there, so another session of yours without one is refused by the Hub. Targets on the same Worker as `minion mcp` are reached by any of your sessions there.
- **Keep rules narrow.** Name sessions, not whole Workers, where you can. Prefer a dedicated shell session for the master over a session where you work yourself. Leave `send_keys` and `answer` out with `--ops` if you only need reads and prompts.
- **The allowlist is a file of your user.** A program that runs as you, including a shell command an agent runs, could edit it or your Claude Code settings. Claude Code asks or its classifier reviews such writes, but they are not impossible. Revoke rules you no longer need, and check `minion claude status` from time to time.
- **Revoke is immediate** for new calls; a call that already left is not recalled.
