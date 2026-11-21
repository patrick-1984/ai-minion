# Claude Code usage integration

On the machine and account that run the Worker and Claude Code, review monitoring and enable the exporter for the selected profile.

```sh
minion worker config usage
minion worker config usage --accept
minion worker config usage integrate claude
```

For a separate Worker state or Claude Code profile, pass `--state-dir` and `--claude-config-dir` to the corresponding commands. The profile defaults to `CLAUDE_CONFIG_DIR`, then `~/.claude`. Start Claude Code with that same profile. Setup edits the profile's user settings. Claude Code controls when settings take effect and which project or managed settings override them. [Claude Code settings](https://code.claude.com/docs/en/settings)

The exporter uses the selected profile's native subscription identity when the local status command provides one. An explicit `--account` can bind an Anthropic subscription fingerprint already shown in Hub Usage. An explicit `--subscription-profile` can give that subscription a common name across Workers when the native identity is unavailable. Use a unique name for each actual account. Without a known subscription identity, the session metrics and activity still work and quota remains unknown. Reconfigure the integration if you change the account used in that profile. The setup tool does not sign in, start a coding session, or request generation. [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference)

## Existing profile settings

Setup keeps the original status-line command in the private `minion-usage.json` manifest, preserves its settings and runs it with the same input, working directory, output and exit-status handling. With no existing status line, the exporter produces no visible output. Existing hook groups stay in their current order. Minion appends separate command hooks for lifecycle events. They remain silent and have no permission or task decision. Session end has a bounded synchronous record attempt so it can finish during Claude Code's shutdown. [Status line](https://code.claude.com/docs/en/statusline) · [Hooks reference](https://code.claude.com/docs/en/hooks)

Repeat the setup command with the same account and profile to restore a missing Minion hook without adding duplicates. Setup refuses unparseable settings, conflicting Minion hooks, disabled hooks in this profile, an unsupported status-line format, and symlinked settings or manifest files. If another settings source overrides the status line or disables hooks, that source still controls Claude Code. On Windows the status-line wrapper uses the configured or standard Git Bash installation, with PowerShell as the fallback.

To remove the integration, edit that profile's `settings.json`. Restore the `statusLine.command` from `previous_status_line` in the private manifest. If there was no original command, remove the Minion status-line entry. Remove the command hooks whose argument list contains `_claude-export` and points to this manifest, keeping the other hooks in those groups. Once the settings reference no managed command, remove the manifest. This same removal procedure allows a different Worker enrollment or account to be installed. An enrollment reset, Hub key change or reassignment fences the previous exporter immediately.

## Collected fields

The Worker uses the same native JSON it receives from Claude Code in a recognized Worker tmux session. Usage includes shared and unshared sessions after the Worker owner accepts monitoring. The allowlisted projection reads model, folder, version, duration, estimated cost, context size, observed input context, and cumulative main-conversation cache writes when available. The five-hour and seven-day windows require an identified Anthropic subscription and only appear when Claude Code reports them. Context figures describe the most recent response. Session-total input and output tokens remain unknown when Claude Code publishes only context gauges. API billing and Claude Gateway spend limits are separate from subscription capacity. [Status payload](https://code.claude.com/docs/en/statusline)

Lifecycle reports cover session start and end, prompt submission, turn completion, subagent start and finish, task creation and completion, working-directory changes, and model switches supported by the installed Claude Code version. The exporter reads bounded native input in memory. It stores only projected telemetry and opaque identifiers. Prompts, transcripts, credentials, task descriptions, model replies, tool arguments and console output stay out of Minion telemetry. Existing status-line scripts still receive their original input. Minion skips oversized or unavailable samples and never injects a command into a coding terminal. [Hook input](https://code.claude.com/docs/en/hooks)

Fixture tests apply the lifecycle hooks and status-line changes to temporary profiles with synthetic enrollment. They cover existing profile settings, command output and exit status, repeated setup, conflicts, bounded data projection, and invalidated grants.
