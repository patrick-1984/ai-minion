---
name: minion
description: "Operate Minion (Hub and Workers that serve tmux, ConPTY or psmux terminal sessions on many machines). Use when the user asks to check, install, start or pair a Minion Worker, to list, read, type into or create terminal sessions through minion, to answer a dialog in a session, to use the minion MCP tools, or to become a Minion master that controls sessions on other Workers through the Hub. Same guide as \"minion llm-help\". Trigger words - minion, minion master, master session, worker, hub, pairing code, session_snapshot, send_text, sessions_list, workers_list, minion control, tmux session on another machine."
---
# Operating Minion: guide for AI agents (Claude Code, Codex and others)

Minion lets people, scripts and agents read and type into terminal sessions (tmux; ConPTY or psmux on Windows)
on many machines. A **Worker** runs on each machine and serves its sessions. A **Hub** is the central server with
the web UI. A Worker works alone (**Solo**) or paired to a Hub. Everything is one binary: `minion`.

A **Minion master** is an agent session that controls sessions on other Workers through the Hub. Nothing is a
master by default: an administrator must give that one session a **session-control grant**. Connected Workers and
their sessions get no control over anything else by default, and a Worker shares no session until its owner
chooses.

## Rules for agents

1. Ask the human before you install Minion, start or pair a Worker, share sessions, or type into a session you
   did not create yourself in this task.
2. Read before you write: `session_snapshot` first, then `send_text`/`send_keys`.
   Some refusals are **forceable** (the result says so, for example `shell_pane`: the pane runs a plain shell, so
   typed text would run as a command): repeat with `--force` only after you reviewed the screen and the human
   asked for that command. Never use `--force` to get past `not_shared`, `forbidden` or a changed pane.
3. Never type secrets (passwords, keys, tokens) into a session.
4. A write prints an outcome. Exit status: 0 delivered, 3 failed_not_sent, 4 partial or unknown, 1 error,
   2 usage. On 4, check the screen before you repeat; repeat on purpose with the same `--request-id`.
5. You cannot grant yourself access: sharing, approval and session-control grants are made by humans in the Hub.
6. `--json` gives machine-readable output on every operation.

## 1. Find out what is here

```sh
minion version                 # is minion installed, which version
minion worker status           # Solo or Hub, Hub URL, connected?, paired to whom, share mode, sessions
minion worker status --json
echo "$TMUX_PANE"              # non-empty: you run inside a tmux session (a Minion session)
```

- `minion` not found: Minion is not installed (step 2).
- Worker status `not paired` with a Hub URL: it shows a pairing code (step 3).
- Worker connected to a Hub and paired: use it (step 4); to become a master, step 5.

### Detect a Worker and read its usage meters

`minion worker status` answers for one profile. To find every Worker this account can see (a service with its own
state directory, root Workers, a stale or stopped one), use discovery. Do not parse files yourself: the command
checks owner, mode, size, schema and future timestamps for you.

```sh
minion worker discover --json --usage
```

Each entry of `workers` has `status` (`running`, `stale`, `stopped`, `unavailable` with `error`), `daemon_version`,
`os_scope` (`user` or `root`), `started_at`, `updated_at`, `updated_age_s`, `hub_origin` and, with `--usage`, the stored
usage-meters document (`minion.usage-meters/1`) as `usage`. Exit status: 0 a Worker is running, 1 none is running,
2 the search failed or a record could not be trusted while nothing runs (a Worker may be hidden). `running` means the
record was refreshed within `stale_after_s` (90 s): a recent-liveness signal, not proof the Hub link is up (use
`minion worker status --json`, `connection.live`). An old Worker that publishes no record appears only when it
answers on the selected profile's socket (`source: socket`). `minion worker status --json` also carries `usage` for the
selected profile when a document is stored. A meter is grey (unknown) once its `expires_at` has passed; never
compute "tokens left" from a percentage.

## 2. Install (only after the human agrees)

```sh
curl -fsSL https://github.com/patrick-1984/ai-minion/releases/latest/download/install.sh | sh     # Linux, macOS
```

```powershell
irm https://github.com/patrick-1984/ai-minion/releases/latest/download/install.ps1 | iex           # Windows
```

The script verifies the checksum of the release and installs `minion` only. tmux comes from the package manager
(Linux, macOS). On Windows, psmux (`choco install psmux` or `scoop install psmux`) gives sessions that survive a Worker
restart.

## 3. Start a Worker and pair it with a Hub

```sh
minion worker up --share-existing none --server https://<hub>       # service (sudo on Linux for a system service)
minion worker up --foreground --share-existing none --server https://<hub>   # in this terminal
```

In the foreground the Worker prints a box `MINION WORKER PAIRING` with a code like `MN-K7Q4-XY12` and its
fingerprint. As a service it pairs in the background: run `minion worker status` (repeat for a few seconds until
it shows the pairing code) to read the code and the Worker fingerprint. Give the code to the human: in the Hub they
open **Workers > Add worker** and enter it. If the Hub asks to approve the
Worker, the human compares the fingerprint with the box. Then the human chooses which sessions are shared, on
the Worker's page in the Hub (**Sharing**). Without `--server` the Worker runs Solo.

`--share-existing` is saved and applied at every start. Once the human shares sessions in the Hub, remove it
(with their consent) so that their choice survives restarts: `minion worker config unset share_existing`.

## 4. Work with sessions

Inside a Minion session the commands reach the local Worker; no address or key is needed.

```sh
minion sessions_list                                  # sessions this Worker shares
minion session_create --name build                    # a new detached session
minion session_snapshot --target build                # the visible screen
minion session_capture --target build --lines 500     # scrollback
minion send_text --target build --text 'make test' --submit
minion send_keys --target build --keys C-c            # named keys: Enter, C-c, Up, Escape, ...
minion set_task --target build --task 'run the tests' # a short label the Hub shows
```

`--target` is `[worker-id:]session[:window.pane]`; without it, the current tmux session.

In `sessions_list`, the `pane` of a session may carry `harness_name`: the name the user gave the conversation in the
harness (Claude Code `/rename` or `--name`). It is a label only, absent when none is known; target sessions by `name`.

Sharing decides what you can reach, locally too: a session that is not shared answers `not_shared`. A new session
is shared only when the Worker shares all sessions; otherwise ask the human to share it (Hub: the Worker's page,
Sharing) before you read or type. Typing into a pane that runs a plain shell is refused as `shell_pane` (forceable,
see the rules): `minion send_text --target build --text 'make test' --submit --force` once reviewed and asked for.

To answer a dialog a pane shows (a permission prompt, a menu), read it first and pass the reviewed pane back, so
the answer reaches only that dialog:

```sh
minion session_snapshot --target build --json > snap.json            # read the dialog; its pane object is .pane
jq -c '.pane | {pane_id: .id, pid, command, session_created, dialog_digest}' snap.json \
  | minion answer --target build --choice 1 --expect -
```

Messages to other agents go through the pipe, which the receiver must accept: `pipe_send`, `pipe_read`,
`pipe_wait` (see the pipe guide).

## 5. Become a Minion master (control other Workers)

Every session is a **slave session** by default: it controls nothing beyond its own Worker. A **master session**
is one that its Worker's owner (or an administrator) nominated, and it reaches only the Workers that accept control
from its Worker.

1. Run in a tmux session on a paired Worker, for example `boss`.
2. Ask the human to open **Access > Minion control** in the Hub: nominate this session as a **master session** on
   this Worker, and on each Worker you should control, make it minion-managed and let it **accept control from**
   this Worker (or from All Workers). You cannot do this yourself.
3. Then, inside that session, without `MINION_HUB_URL`:

```sh
minion workers_list                                   # Workers you may reach, with their ids
minion sessions_list --hub-worker <worker-id>
minion session_snapshot --target <worker-id>:<session>
minion send_text --target <worker-id>:<session> --text 'git pull' --submit
```

Name other Workers by id. A target that does not accept control from your Worker answers `not_found`; a session
that is not a master session gets `forbidden`. **Check access** on that page explains any refusal.

`minion workers_list` lists every Worker the Hub lets your session read, which can include your own Worker, and its
result has no `not_listed` field: try a target to learn whether it accepts control. Only the scoped master call of a
nominated master session (not this CLI command) lists just the Workers that accept control from it and reports
`not_listed`, the number of Workers the Hub shows you that grant you no control right now; a Worker whose policy
grants you nothing is neither listed nor counted there. `sessions_list` has no
`--target`: use `--hub-worker <worker-id>`. The administration operations (`control_check`, `hub_diagnostics`,
`usage_list`) are not forwarded by a Worker: they need a Hub key (section 7), because a session holds no right to them.

## 6. MCP tools

```sh
claude mcp add minion -- minion mcp                       # read-only tools
claude mcp add minion-rw -- minion mcp --allow-writes     # with send_text, send_keys, set_task, answer
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.minion]
command = "minion"
args = ["mcp", "--allow-writes"]
```

## 7. From outside a Worker: the Hub API with a key

```sh
export MINION_HUB_URL=https://<hub>
export MINION_API_KEY_FILE=~/.config/minion/api.key   # a key the human created in the Hub (API keys)
minion workers_list
minion session_snapshot --target <worker-id>:<session>
```

## 8. When something fails

- Add `-v` to any command for debug logs; `minion worker status` shows the last connection error.
- `minion hub_diagnostics` (with a Hub key) shows how the Hub sees the request.
- `minion <command> --help` explains every flag; `minion --help` lists all commands.
- Documentation: https://github.com/patrick-1984/ai-minion (README, docs/guides).
