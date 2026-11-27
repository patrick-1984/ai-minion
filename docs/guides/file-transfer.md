# File transfer

Back to the [README](../../README.md).

From v0.24.0 Minion copies files and folders between the folder of a session on one Worker and a caller: another
session's folder, the folder where you run the CLI, or your browser. The Hub admits every transfer, relays the bytes
and keeps the history; it never stores file content.

This guide covers how to use it, what is allowed, the limits, the off switch on each Worker, and the security
trade-offs. The design is in the file transfer BRD (files.v1).

## What a transfer is

- **Pull**: copy a file or folder from a session's folder on a target Worker to the caller.
- **Push**: copy a file or folder from the caller to a session's folder on a target Worker.
- **The folder of a session** is the current folder of its pane when the request arrives (`pane_current_path`, checked
  against the pane process's real folder). Paths in a request are always relative to that folder. Nothing outside it
  is read or written: no `..`, no absolute paths, no symbolic links (neither the file itself nor a folder on the
  way), no other file systems mounted below it.
- **Linux, macOS and Android** Workers are transfer ends. A Windows Worker in this version answers every file request
  with `files_root_unknown` and does not offer file transfer. On Android every transfer uses the app's private
  folder `Minion files` (see the [Android README](../../android/README.md#file-transfer)).
- A Worker older than v0.24.0 does not announce the feature: the Hub answers `unsupported_op` and the web UI shows no
  Files panel for it. Update the Worker to use file transfer with it.

## How to use it

### From the command line

With a Hub configured (`minion` uses the Hub's REST API with your API key), the CLI process is the caller end: it
sends and receives in its current folder.

```sh
# List a session's folder (paginated: --cursor, --limit).
minion files ls --target build-box:api src

# Pull a file or a folder into the current folder (--to renames it inside this folder).
minion files pull --target build-box:api dist/app.tar
minion files pull --target build-box:api logs --to logs-build-box

# Push a file or a folder into the session's folder (--as names the destination there).
minion files push --target build-box:api ./fix.patch --as patches/fix.patch
minion files push --target build-box:api ./assets
```

- `--target` is `worker:session` and must name a Worker (a session code such as `5-667` works too).
- An existing destination file is never replaced unless you pass `--overwrite`, and only when it is a regular file.
  A pushed folder is merged into an existing folder; an existing file in it is a conflict.
- Exit status: 0 delivered, 3 nothing was published (`failed_not_sent`), 4 partly published or unknown, 1 error,
  2 usage. Progress goes to stderr on a terminal; `--json` prints the result.
- Ctrl-C stops the transfer; nothing is published at the receiving end.

Inside a session of a Worker (no Hub configured in that shell, the CLI talks to the local daemon) the caller end is
that session's own folder. That path needs the calling session to hold file authority through the Hub: see
[From a session on another Worker](#from-a-session-on-another-worker).

### From a session on another Worker

A session (an agent or you in its shell) copies files between its own folder and a session on another Worker with
the same commands, run inside that session: `minion files ls|pull|push --target worker:session ...`, or the MCP file
tools of `minion mcp`. Its Worker's daemon moves the bytes; the session's folder is the caller end.

The Hub gives a session file authority only for a **Verified** source: the session's terminal incarnation holds a
bound lease on the master lane. That is the case when

1. the session is a prepared Claude Code terminal (`exec minion master claude` as the pane's process, Linux; see
   [AI agents](agents.md#prepare-one-claude-code-terminal)) and is either made the bound master in Minion control
   or has its direct grant **bound** to that terminal (the `grant_bind` op); and
2. the authority names the file action: the grant (or, for a bound master, the target Worker's master
   acceptance) has **files_pull** (list and pull, level View or higher) or **files_push** (push, level Control)
   ticked. Both are off by default, and a grant must be bound before the flags can be set.

The daemon then sends the request on the master lane, and only when the command runs inside that pane (its process
descends from the pane's process); the Hub decides it on the grant or acceptance flags, re-checks it every 15
seconds while bytes move and once more before the receiver publishes. Revoking or unbinding the grant, ending the
master, removing a flag or unsharing a session stops a running transfer and publishes nothing.

A session without a verified lease is only named by its Worker (a **Claimed** source). The Hub refuses its file
requests with `forbidden` and the message says what to do: bind the session as a master or bind its grant, and tick
`files_pull` or `files_push`. A Hub older than v0.24.0 answers `unsupported_op`.

```sh
# Owner: grant boss on box-a Control on build on box-b (grant_set), bind it to the
# prepared terminal of boss (grant_bind), then set files_pull and files_push (grant_set).
# Inside the pane of boss on box-a (an agent's tool or the shell below it):
minion files pull --target box-b:build dist/app.tar
minion files push --target box-b:build ./fix.patch --as patches/fix.patch
```

### From an AI agent (MCP)

`minion mcp` offers `file_list`, `file_pull` and `file_push` through the local daemon: the agent names paths only,
and the daemon moves the bytes. With a Hub REST backend `minion mcp` offers only `file_list`; pulls and pushes run
through the CLI, so a model never sees a leg token.

### From the browser

The session page has a **Files** panel for sessions you may transfer from: browse, download a file (resumable) or a
folder (`.tar.gz`), and upload a single file. The **File transfers** page lists the history of transfers that touch
your Workers or that you started (administrators: all).

## Who may transfer

Every transfer is decided at both ends, at admission, every 15 seconds while bytes move, and once more just before
the receiver may publish (the commit gate). A refusal at any of these points publishes nothing.

| Caller | File authority |
|---|---|
| Administrator (browser) | every session |
| Owner (browser, act-as) | own sessions |
| API key | only the file actions the key **names**: `file_read` (list, pull) and `file_write` (push). A key with "everything" or a wildcard scope, and every key created before v0.24.0, has no file action. |
| Session on another Worker | only through a **Verified** grant or master acceptance with the flag `files_pull` or `files_push` (default off): a grant bound to the session or a bound master, requested on the master lane ([how](#from-a-session-on-another-worker)). A Claimed source never carries file authority (`forbidden`). `files_pull` needs level View or higher, `files_push` needs Control. |
| Worker identity | none |

On top of the caller's authority:

- **Hub policy** `file_transfer_policy`: `on` (default), `pull_only`, `push_only`, `off` or `admins_only`, named from
  the caller that starts the transfer. Set it in the Hub's settings or with `MINION_FILE_TRANSFER_POLICY`. A change
  applies at once, also to transfers that are running.
- **The Worker's local switch** (next section) of each Worker end.
- **Sharing**: the target session, and a calling session, must be shared with the Hub.
- **Revocation**: revoking an API key, a grant or an acceptance, or switching the policy or a Worker off, stops a
  running transfer at the next decision; nothing is published.

## The off switch on each Worker

The Worker setting `file_transfer` is local: the Hub cannot change it or lift it.

```sh
minion worker config set file_transfer off            # refuse every file request
minion worker config set file_transfer send_only      # files may leave, none arrive
minion worker config set file_transfer receive_only   # files may arrive, none leave
minion worker config set file_transfer on             # default
```

The environment variable `MINION_FILE_TRANSFER` works too; a narrower value in the config file holds against a wider
one in the environment. A change applies to the next request and stops a publish that has not happened yet. A
refused request gets `files_disabled`. On Android the checkbox **Allow file transfer (Minion files folder)** sets it.

Two more local relaxations, both off by default:

- `file_transfer_allow_home=true` allows a session whose folder is the home folder itself. Sensitive folders such as
  `~/.ssh` stay refused.
- `file_transfer_protected=off` lets pushes write the protected names below. It is recorded in the transfer history
  (`protected=off`). System, home and sensitive folders stay refused either way.

## What is refused

- **Folders that are never a transfer folder**: `/`, system folders (`/etc`, `/usr`, ...), the home folder itself
  (unless allowed above), sensitive folders and the Worker's state folder.
- **Push-protected names** (`path_protected`), matched per path element: `.git/hooks/`, `.git/config`, `.hg/`,
  `.svn/`, `.envrc`, `.env*`, shell rc files (`.bashrc`, `.bash_profile`, `.profile`, `.zshrc`, `.zprofile`,
  `.zshenv`, `config.fish`), `.ssh/`, `.gnupg/`, `.npmrc`, `.pypirc`, `.netrc`, agent configuration (`.claude/`,
  `.mcp.json`, `.codex/`, `.cursor/`, `.vscode/tasks.json`, `.vscode/settings.json`), CI definitions
  (`.github/workflows/`, `.gitlab-ci.yml`). `Makefile` and other build files are not protected.
- **Pull-sensitive names** (`path_sensitive`; a folder pull skips them with reason `sensitive`): `.ssh/`, `.gnupg/`,
  `.aws/`, `.netrc`, `.git-credentials`, private keys `id_*` (not `.pub`), `*.pem`, `*.key`, the Minion state
  folder. `.git/` itself can be pulled.
- **Entry types**: only regular files and folders. Symbolic links, devices, sockets and FIFOs are skipped in a folder
  (`skipped_reason`) and refused as a single path (`file_type_refused`).
- **Paths** outside the folder (`path_outside_root`): `..`, absolute paths, empty or `.` elements, a symbolic link on
  the way.

Nothing received is ever executable: files are written with mode 0644 (minus the umask), folders 0755; ownership,
setuid bits, extended attributes and ACLs are not copied.

## Limits

Defaults; the Hub and each Worker can lower them, and the lower one wins.

| Limit | Default |
|---|---|
| Logical bytes per transfer | 4 GiB |
| Entries per transfer | 100,000 |
| Folder depth / path length / name length | 32 / 4096 bytes / 255 bytes |
| Manifest | 16 MiB |
| Running transfers | 2 per Worker, 4 per user, 32 per Hub |
| Admissions | 20 per minute per Worker |
| Idle data leg | 60 s without body bytes |
| Time to open the data leg after admission | 60 s |
| Free space on the receiver | refused when less than 5 % or 1 GiB would remain |

A refused limit gets `transfer_too_large`, `rate_limited` or the limit's own code. A transfer holds its running slot
until it ends, which for a pull includes the receiver's receipt.

## Resume

A single regular file that has not changed for 2 seconds gets a strong ETag. An interrupted download of such a file
continues with a new admission and `Range` plus `If-Range` with that ETag: the Worker answers 206 with the rest of the
file, and the `Repr-Digest` trailer carries the SHA-256 of the whole file for the client to check. A file that changed in between gets 200 with the whole file. Folders,
compressed transfers and files that keep changing restart from the beginning.

## Integrity and outcomes

The receiver writes into private staging files, checks every file's SHA-256 and size against the sender's manifest,
the manifest's digest, the end of the archive and the HTTP trailers, and publishes only when everything matches
(atomic, never replacing an existing name unless asked). Any mismatch gives `integrity_failed` and publishes nothing.

| Outcome | Meaning |
|---|---|
| `delivered` | everything was verified and published |
| `failed_not_sent` | nothing was published |
| `partial` | some files of a folder were published before a failure (the result lists them) |
| `unknown` | the receipt of the receiver was lost; a late receipt resolves it |

## Security trade-offs

- **File transfer is on by default on v0.24.0 Workers**, but no caller has file authority until you give it: API keys
  need the named actions, grants and acceptances need the flags, and only Verified sessions qualify. Owners and
  administrators can transfer on their own sessions from the browser. If a Worker should never send or receive files,
  set `file_transfer off` on it: the Hub cannot turn it back on.
- **The folder of a session is what the pane is in now.** An agent in a pane can `cd` before a transfer. Keep
  sessions that may receive files in project folders, not in `~`.
- **A compromised Hub** can start transfers within what each Worker's local rules allow: shared sessions only, inside
  their folders, never protected names, never with the switch off. It cannot read process memory, run what it
  writes, or write outside a session folder.
- **Exfiltration by an agent with a grant** is bounded by the flags, the policy, the sensitive list and the limits,
  and visible in the history (who, which session, which grant, the counts and the manifest digest). Transfer records,
  audit events, logs and metrics never contain file names, paths, tokens or content.
- **Pushed content can still be dangerous when used**: a pushed script is not executable, but a person or an agent
  can still run it. The protected list covers the places that run code by themselves (hooks, rc files, agent and CI
  configuration); `file_transfer_protected=off` removes that protection.
- **Resume trusts the file's identity** (device, inode, size, change and modification times): a client that resumes
  should check the whole file against the `Repr-Digest` trailer, as the Minion ends do.

See also [Access: who reaches what](access.md), [API keys and MCP](api-keys-and-mcp.md) and the
[security overview](../security.md).
