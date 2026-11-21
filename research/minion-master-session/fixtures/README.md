# A77 Claude Code native qualification

This fixture runs Claude Code 2.1.330 on Linux in private tmux with a temporary shared profile and working directory. In each pair the prepared process gets an extra stdio server via inline `--mcp-config`. Both processes have the same synthetic baseline tool. The actual vendor CLI sends each model request to a loopback Messages endpoint. The fixture records tool names and catalog status from those requests, native output, and MCP responses. It uses no real account, paid inference, active harness, or installed profile. The local broker stands in for authority. It has no Hub or Worker implementation.

## Observations

| Probe | Result |
| --- | --- |
| Native print stream | Both sessions start with the baseline tool. Nomination makes `master_list` available only on the prepared invocation and the fixture sees one successful native MCP call. The sibling's model-facing tool catalog is identical across the transition and on revoke. |
| Healthy revoke | Claude re-fetches an empty master catalog. Print-stream `system/init.tools` drops the master tool. The interactive `/mcp` panel shows `minion_fixture  no tools · connected` and retains the unrelated baseline tool. The registration itself stays in the prepared invocation. |
| Historical model request | Later requests can include the old tool declaration even after the current catalog withdraws it. This occurred even when no tool had been used. A fake model's stale tool use after withdrawal returns an error without a second delivered call. |
| Failed rediscovery | A deliberate `tools/list` error leaves Claude's prior master tool available in its cached list. A native call through that cached tool reaches the fixture broker and is denied after revoke. A later healthy refresh withdraws the current tool. |
| Live identity | The real stdio helper is a direct child of the pinned vendor process. Linux `SO_PEERCRED`, executable identity, process start times and pidfds identify the peer independently of a claimed pane. A sibling fixture with copied pane metadata is refused. Joining a second tmux session to the same window and moving the window each retire the binding. Unlinking and same-name recreation do not restore it. Replacing the harness within the same pane also retires it. |
| Local broker reconnect and chat clear | The surviving verified helper withdraws its catalog when its broker becomes unreachable and re-fetches after the broker accepts it again. In print stream `/clear` changes the native chat ID while keeping the same terminal attachment. |
| Shared settings | The fixture's shared settings file is byte-identical before and after. The interactive test uses only synthetic input in the two test-owned panes. |

Use the next observed completed native turn after discovery as the presentation boundary. A response to an MCP list request does not prove the client has applied it to a concurrently starting turn. The offline and terminal-change probes sometimes needed several turns for withdrawal to become visible. Hub revocation must precede catalog convergence. The reconnect probe keeps the same broker process and pinned helper while reopening its listener. A fresh native helper, Hub reconnect and Worker restart are separate cases for implementation. Historical model declarations can remain, and the unprepared running process still has no session-local attachment path.

## Reproduce

Install the Linux vendor CLI 2.1.330, Go and tmux. Run from the Minion checkout on a kernel that supports pidfds and Landlock ABI 4 or later. The test refuses to start a vendor session if its confinement fails. The launcher uses a fresh explicit environment, a dummy API credential, a temporary `CLAUDE_CONFIG_DIR`, filesystem restrictions, and TCP restricted to the fixture port. The API fixture listens on the loopback address. Interactive probes use the vendor's test-appropriate demo mode for a temporary profile. Structured print tests send JSON through private fixture files.

The recorded run used Linux x86-64 7.0.0-38-generic, tmux 3.6 and vendor CLI SHA-256 `f390eaa0814e6a1bff74cbb47acb74cb9c89d30ced809fe0af5af00c7bfe8ebb`.

```sh
MINION_REQUIRE_TMUX=1 go test -race -tags=a77_native \
  ./research/minion-master-session/fixtures \
  -run '^TestClaude(Native|Interactive)$' -timeout=2m -count=2 -v
```

The opt-in tests preserve existing workspace tmux and look only at PIDs obtained from their own terminal hosts and local fixture sockets. Cleanup ends these child processes with their pinned descriptors and removes the temporary fixture directory. The mock model never persists model requests, prompt content or profile data. [Claude CLI reference](https://code.claude.com/docs/en/cli-reference), [MCP live updates](https://code.claude.com/docs/en/mcp#dynamic-tool-updates), [Claude environment variables](https://code.claude.com/docs/en/env-vars), [Messages streaming](https://platform.claude.com/docs/en/build-with-claude/streaming), [architecture decision](../ASTRA-DECISION.md).

## Production Hub and Worker verification

The production probe exercises the real `minion master claude` launcher in print-stream mode against the default Hub and two Workers built from the checkout. Two prepared Claude processes and an unprepared sibling share the same isolated profile and project. Only the nominated terminal receives tools. The model endpoint stays synthetic and on loopback.

- A real administrator nominates, revokes and re-nominates through `control_set_bound_master`. The current catalog empties after revoke. Invocations from a held native turn and the older catalog generation fail. The unrelated catalogs and settings stay unchanged.
- The scoped tools return no targets without consent. With consent they list and inspect the shared agent while refusing the source shortcut and a private destination. The destination records exactly one received prompt after two calls carrying the same request ID. A separate ordinary administrator can still list both Workers.
- A replacement Hub link recovers the same surviving terminal after it proves the current nomination again. Share loss and Worker replacement retire the preparation. A caller with copied public generation data and a legacy name-only mutation cannot take over the bound role.
- The companion authenticated protocol client deliberately keeps its issued lease while skipping revocation updates. The real Hub denies it after revoke, after identical re-nomination, on a new authenticated link, and after expiry on a live link. The native failed-rediscovery case above uses a fixture broker. The production probe holds a native turn across the actual revoke, and the companion checks real Hub authority independently of native catalog behavior.

```sh
MINION_REQUIRE_TMUX=1 go test -race -tags=a77_native \
  ./research/minion-master-session/fixtures \
  -run '^(TestClaudeProductionMaster|TestProductionHubRejectsStaleMasterLease)$' \
  -timeout=2m -count=1 -v
```

Use Linux with `SO_PEERPIDFD` for the production probe. The companion waits through the real 30-second lease deadline. Its protocol exchanges use generated fixture identities and never log lease values. The Hub listens on a loopback port and the two Worker processes have separate state and runtime directories.
