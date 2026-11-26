# Changelog

## Unreleased

## v0.21.0

### Added

- **Session grants.** One session may reach a session on another Worker (or every shared session of that Worker, future ones included) at a level: **View** (`workers_list`, `sessions_list`, `session_snapshot`, `session_capture`), **Message** (also `send_text` as a submitted prompt to an AI agent pane: the Hub makes the send agent-only, requires `submit`, refuses `force`, and answers `unsupported_op` when the target Worker does not announce the agent-only send) or **Control** (also `send_text` to any pane, `send_keys`, `set_task`, `answer`). The owner of both Workers decides between their own Workers, an administrator between users. Creating, changing and approving a grant need a signed-in browser session: an API key, a Worker, a session and an act-as session are refused. A grant between two sessions of one Worker is refused. Master sessions and "accepts control from" work as before, and with no grants the Hub decides as v0.20.6 did. Hub-only: no Worker update is needed (migration 0032, table `session_grant`).
- Operations `grant_list` (an owner sees the grants that touch their Workers, an administrator all), `grant_set` (create, or change level, expiry and note with `id` and `if_revision`), `grant_revoke` (the owner of a Worker it touches, an administrator, an API key with `operate` on one of the Workers) and `grant_approve` (administrator). A change made for an outdated revision answers `conflict`.
- A grant ends by itself: grants that name a session are deleted when the session ends, is renamed or stops being shared; every grant of a Worker is deleted when the Worker is removed or changes owner. A grant that an administrator decided is **suspended** when that administrator loses the role or is deactivated, stays suspended when the role comes back, and works again only after an administrator approves it. An expired grant stops working at once and is deleted 30 days later. The Hub reads the grants on every call; a running call is checked again within 30 seconds and ends when its grant was revoked, changed, suspended or has expired.
- **Access** (`/access`, every signed-in user): the matrix "Who reaches what" shows which session reaches which session on another Worker right now, through a master session or a grant, the level in effect, the layer that blocks a pair (suspended, expired, authority, owner rule, Worker pending, not shared, Worker offline), and whether the source is Claimed (a name its Worker reports) or Verified. It has Refresh, an "As of" time, a filter, a print layout and a list form on narrow screens, and changes nothing. A user who is not an administrator sees the pairs with one of their Workers at either end; a session of another owner appears as "a session of <owner>". The same as the operation `access_matrix`.
- **Access > Grants** (`/access/grants`): the list of grants with source, target, level, state, who decided and until when; add a grant (source, target or every shared session, level, no end / 7 days / 30 days, note), change its level, revoke it, and approve a suspended one (administrators).
- **Access > Why?** (`/access/why`) and the operation `access_explain`: for one source and one target, every check in order (caller, grant, authority, time, sharing, Workers, preview-only) as pass, fail, not evaluated or unknown. Matrix cells link to it.
- Audit: grant changes are recorded as `grant.create`, `grant.update`, `grant.approve` and `grant_revoke` with the target, the source session, the grant id and a k=v detail, refusals included.
- Guide [Access: who reaches what](docs/guides/access.md).

### Changed

- Audit: events carry a source session, a grant id and a k=v detail (migration 0033). Minion control changes record what was set, brokered writes name the calling session, and a refused brokered write is recorded as `denied` instead of `failed`.
- A session that holds direct grants gets a `sessions_list` answer cut to the sessions it may read.
- Calls that a session makes through a grant or as a name-only master are decided once more just before the request is written to the target Worker ("the grant was removed or changed"), and a running call ends after two failed re-checks in a row instead of continuing.
- Access > Minion control uses the whole window, shows every Worker row without an inner scroll box, names the owner in the matrix, and has a Refresh button, an "As of" time and a print layout.
- The guides [Minion control](docs/guides/minion-control.md) and [Session control](docs/guides/session-control.md) point to the new guide; statements that no longer hold (creating a 0.8.0 grant in the web UI, "Add a master session") are corrected.

## v0.20.7

### Fixed

- Minion control: a name-only revoke (`control_set_master` with `master=false`, `session_control_set` without targets) of a session that holds an active session-bound master is refused with a conflict instead of answering delivered and revoking nothing; the check runs in the same transaction as the delete.
- Access > Minion control: an acceptance limited to some sessions shows "limited (n)" and an acceptance that allows nothing shows "blocked" (also on a Worker that accepts all Workers); a tick on a limited row keeps its list instead of widening it to every session; a failed "Who can control this session?" check says so instead of showing "no master session".
- Minion control: withdrawing "accepts control from" for one Worker on a Worker that accepts control from all Workers no longer widens that source to every shared session (the Hub keeps a row that allows nothing); `control_set_master` and `session_control_set` refuse to revoke a session-bound master by name instead of answering delivered and revoking nothing; `control_check` reports a store error instead of leaving the master out.
- Access > Minion control no longer shows a plain tick for an acceptance that is limited to some sessions (badge "limited (n)") or that allows nothing (not ticked, badge "blocked"); ticking a limited acceptance no longer replaces its limit.

## v0.20.6

### Fixed

- Linux Worker started at shell login (installs without systemd): interactive shells no longer print `[1]`, `[2]` and `Done ( IFS= read -r minion_bin && exec "$minion_bin" worker start --at-login ... )` job lines. The login block now starts the Worker in the background of a subshell, which creates no job in bash, zsh or dash. A guard holding the shell's PID makes the block run once per shell when `~/.profile` sources `~/.bashrc`, while every other shell (a child shell, a tmux pane, also with `set -a`) still checks the Worker. New installs get the new block. Existing installs: the Worker never changes a startup file (not `worker run`, `worker start` or a self-update). When a startup file still holds the block of v0.20.5 or earlier, the Worker logs it once at start and the Hub's Worker page shows "Login startup is outdated — run `minion worker up` on this host to remove the 'Done' messages in new shells". `minion worker up` replaces that exact block (byte for byte between its complete marker lines) in `~/.profile`, `~/.bashrc`, `~/.bash_profile`, `~/.bash_login` and `~/.zprofile`: it writes the new version to a new file with the same group and mode, syncs it and renames it over the original, and keeps the displaced version as `<file>.minion-backup` (`.minion-backup.1` and so on; mode 0600). It leaves a file as it is and prints a one-line repair (delete the old block between the marker lines, run `minion worker up` again) when the file is a symbolic link, has extended attributes or an ACL, has other hard links, belongs to another user, is in a directory with a default ACL, or changed (owner, group, mode, size, modification time or content) while it was updated. Such a file never stops `minion worker up`, and it no longer adds a second block.

### Changed

- `send_text` to an agent that is busy with a turn is no longer refused (without `force`). The text is typed into the input line and the submit key is **never** pressed automatically, for every agent, Claude Code included: a permission dialog can open between the Worker's last look at the screen and the key, and an Enter would answer it (for example approve a Bash command); no screen check can close that gap. With `submit` the outcome is `partial`, reason `busy`, warning `session_busy`, and the note "session busy: the text was typed into the input line but not submitted, so it cannot answer a permission prompt by accident; when the session is ready, check the input line there and press Enter (do not send it again — it would be typed twice)". Without `submit` it is `delivered` with the busy note. A multi-line text is typed into a busy pane only for Claude Code and Codex, which enable bracketed paste (tmux and ConPTY bracket a paste only for a program that enabled it); into any other busy program nothing is typed, because a line-oriented program could submit the first line: `failed_not_sent`, reason `busy`, forceable, note "session busy: multi-line text was not typed, because this program could submit it line by line; send it when the session is idle (or with force=true and a new request_id)". The Hub's session page shows "Typed, not submitted — the session is busy; press Enter in the session." and keeps the draft in the box and as the recoverable copy, as for any partly sent text. `force=true` behaves as in v0.20.5. `send_keys` to a busy agent is still refused as `busy` (forceable). The Worker audit records such a send as `partial` (or `delivered`) with reason `busy`; a delivered one is not a failure.
- Every refusal of a write carries a `note`: what stopped it, why, and how to send anyway (for example force=true with a new request id). The CLI prints it (`note:`), MCP results include it (derived for a Worker older than this release), and the Hub's session page shows it under the result; text sent into a busy agent is shown there as an information notice, not as a failure.

## v0.20.5

### Changed

- Hub settings `listen` (`MINION_LISTEN`) and `management_listen` (`MINION_MANAGEMENT_LISTEN`) take a plain port number: `8443` means every interface (the default is now shown as `8443` instead of `:8443`), and a plain `management_listen` port such as `8444` means `127.0.0.1:8444`. A plain port is digits only. The forms `:8443` and `host:port` keep working exactly as before, including the values v0.20.4 accepted such as `127.0.0.1:+9443` or a long zero-padded port: an upgraded Hub never refuses to start on a value v0.20.4 started with. The TrueNAS updater and the self-update readiness probe read the port the Hub binds from every form.
- Browser sessions: `session_ttl` (`MINION_SESSION_TTL`, **Administration > Hub settings**) is now the **idle** lifetime, default **2 hours** (was a fixed 24 hours from the sign-in). A request the person makes on the Hub's own pages extends the session: the browser must show that (Fetch Metadata `Sec-Fetch-Site: same-origin`, or `none` with `Sec-Fetch-User: ?1`; without Fetch Metadata an `Origin` or `Referer` equal to the Hub's public origin). Timed polls, refreshes after live events, live streams and their re-checks, the routes pages poll (also from tabs opened before the upgrade), requests from other sites and requests without such evidence do not. A local password session ends 12 hours after the sign-in whatever the activity; an OIDC session 7 days after it; these caps do not depend on `session_ttl`, and a `session_ttl` above 12 hours acts as 12 hours (also for the first idle end of a new session). A lowered `session_ttl` applies to open sessions on their next request; sessions of v0.20.4 are clamped the same way on first use. The session cookie now lives until that absolute end (the Hub decides validity).
- When the idle lifetime of an OIDC session has run out, the person's next page load on the Hub (a refresh, a link, the address bar; never htmx, a fetch, a poll or a stream, and never a page load another site caused) goes through the identity provider with `prompt=none`: while the identity provider's own browser session is valid it answers without any interaction, the Hub signs in through the ordinary sign-in path (a new session and cookie, the role rules applied, audited as `login`) and returns to the same page. When the identity provider answers `login_required`, `interaction_required`, `consent_required` or any other error, the Hub's sign-in page is shown and returns to the same page after the sign-in. Nothing extends or renews an expired session: ending the person's browser session at the identity provider prevents a later silent sign-in (a Hub cookie in foreground use keeps working up to its absolute end; deactivate the user in the Hub to end access at once). Every other request of an expired session gets a quiet `401` with `X-Minion-Session: idle` (`reason: session_idle`); pages show "Session paused" with a Continue button that reloads the page (every other control, Sign out included, keeps working as it is). The inspector and the terminal view keep their frames, drafts and screen (hidden) on any missing sign-in, expired or absent cookie alike, and destroy nothing without the person; a paused terminal sends again only after a fresh read whose server-issued principal context matches the view. A terminal no longer reloads itself on Worker events while nobody uses it (a page load would extend the session): it shows "Out of date" and reloads on the next click. The sign-in buttons of the login page keep the return path. See [browser sessions](docs/security.md#browser-sessions).

## v0.20.4

### Added

- Sessions and Worker pages: a **Name** column shows the name you gave a Claude Code conversation with `/rename` (sortable, searchable). The Worker's Claude exporter shows the status line's session name only on evidence that you typed it: the transcript holds the record of your own `/rename <name>` command (only those records are decoded, searched incrementally in bounded steps, nothing else of the transcript is kept). An AI-generated title, the title an unnamed `/branch` derives from the first prompt, a title set by a prompt hook and a name given with `claude --name` are not shown. A new name is shown only once confirmed; until then (also while the transcript cannot be read) the session shows no name, and an older status-line callback that finishes late never brings back a name; a conversation that ended (`/clear`) never shows its name. The name travels only on the Worker's session state stream to its Hub, for the Hub's pages: `sessions_list`, `session_snapshot`, `session_capture` and the CLI do not report it. On the Hub a name is shown only when the read of the session was authorized under the owner whose Worker connection reported it (one ownership snapshot, also for the event stream, whose name-bearing events are authorized afresh): after a reassignment neither owner sees the other's names. Codex thread names are not shown: Codex stores automatic titles in the same place. A long name wraps (14 to 28 characters wide). The Sessions table switches to the card layout by the width of its own box instead of the window (cards below 1240 px, or 1320 px with the Owner column), so the rail, the inspector or the Name and Owner columns never make it scroll sideways; in the card layout an empty Name is left out and the hidden column header with its sort buttons is out of the Tab order (a phone keeps its sort bar). Needs an updated Worker; older Workers show an empty Name.

## v0.20.3

### Fixed

- Sessions and Worker pages: the session table shows the state details in their own columns instead of stacking them in the State cell: **State** (the badge), **Activity** (for example `2 shells` or `1 shell, 1 subagent`), **Source** (`Claude Code hooks`, `Codex hooks`, `Terminal screen`) and **Last event** (time and how long ago). The new columns sort (Last event to the millisecond). From 1280 px each session is one line; below that, where the table would not fit beside the menu, each session stacks as a card with labelled fields and leaves out empty details (an empty-list message stays visible).

## v0.20.2

### Fixed

- Hub logs: every audit event (sign-ins and failures, settings, invitations, API keys, Worker approval, role changes, session control) is also a structured log record at INFO, or WARN for a denial or failure, with actor, target, outcome and request id; the syslog and OpenTelemetry sinks at level info now carry them. Responses carry `X-Request-Id`.
- Audit: one row per change (the dispatcher no longer adds a second row for an operation that audits itself), and the audit page shows user and Worker names next to the ids.
- A write that someone makes on a Hub-connected Worker's own session through the local socket (for example `minion send_text`) is reported to the Hub audit as `local.<op>` (actor `worker:<id>/local`, only a resolved session, known outcomes and reason codes, a per-Worker budget; a cached duplicate is not reported again; a v0.20.1 Hub gets no report), and the session's Details show harness, model and usage when known.
- Diagnostics shows the log sinks' last success, last error and drop reasons, and says which `MINION_TRUSTED_PROXIES` value fixes a wrong client IP behind a proxy. `MINION_LOG_HOSTNAME` sets the host name of exported logs.
- Usage: an estimated cost of zero without any token count shows Unknown, a duration says whether the harness reported it or the Hub observed it, and a missing Codex model says why.
- Usage: a Claude subscription account no longer flips between an idle session's old reading and an active session's current one (for example weekly 4% without a 5-hour window, then 20% with 5-hour 3%). Idle sessions re-send their last limits every few seconds; such a re-send keeps the time the limits were first seen, and the Worker, the Hub and the read-back merge each window across sessions and Workers (the later reset wins, then the higher used share). The card says **No 5-hour window reported** only when recent active reports lack it (it does not claim a plan property: a capped plan reports the window again once one is open); otherwise the 5-hour limit shows as unknown.
- Usage: API-key and custom-endpoint account totals are split into **Running now** and **History · last 90 days** (which includes ended sessions), so history is not read as current spend. A Claude run that ends inside an open tmux session (`SessionEnd`) is shown as stopped, and a re-sent report never reopens an ended session unless it has the same start time.
- Usage: a passive collector with recent limits shows **Passive · from Claude status line** and the time of the last sample instead of **Waiting for native observations**.
- Master CLI: `workers_list` reports `not_listed`, and the help of Hub-only operations and `sessions_list` says how to reach another Worker.
- Shared sessions report native Claude Code activity, including background shells and subagents, and Codex turn activity. The Worker and Sessions pages show the observation source and last native event time.

## v0.20.1

### Fixed

- Claude Code usage: the Worker now reads the whole status line — tokens, cost and the account's limit windows. A plan without a 5-hour limit shows only the windows it has.
- Sessions on an API key, an API key helper or a custom endpoint, set in the environment or in the profile's settings, are shown as **Custom endpoint** or **API key**. Their limits stay with that access; they are never attributed to a subscription login. The subscription account is bound automatically, also for Workers set up by v0.20.0.
- A Worker update that cannot run, for example because the release is not published yet, shows the reason on the Workers page. The audit records it as failed. **Update** is disabled while the Hub's release is not published.
- The Claude exporter state is current on every report, and a manual scan or Refresh bypasses the Worker's cached harness scan.

### Added

- **Last checked** and a **Refresh** button on the Usage views, the account cards, the Harnesses page, the Worker page and Updates. A refresh asks the Worker for fresh data, at most once every 10 seconds per Worker.

## v0.20.0

### Added

- Harnesses page (Workspace → Harnesses): tick the coding harnesses you want (Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, Aider, Cline), check all your Workers with one click and see which have each one installed or missing.
- Claude Code usage without manual steps: once usage monitoring is accepted, the Worker adds its status-line exporter to each Claude Code profile (the existing status line keeps working; managed settings are left alone), so session tokens, cost, model and the 5-hour and weekly limits are reported. Turn it off with `usage_claude_exporter=off`; `minion worker config usage integrate claude --remove` restores the previous status line.
- Sessions on an API key or a custom endpoint are recognised: each session shows its access (subscription, API credential, or custom endpoint with its host; key values are never read), and API and endpoint accounts show token and cost totals.

### Changed

- The Windows join command carries the join token like the Unix commands, and the join token is shown above the commands on Add worker.
- Account e-mail addresses are shown in full to the Worker owner and Hub administrators.
- Limits have readable names ("Weekly limit", "5-hour limit") instead of internal identifiers.
- Session lists use natural order (wb-2 before wb-10).
- Usage → Workers shows the Claude exporter state per Worker with the exact fix; stale accounts say why.
- The Updates page explains how to set up automatic Hub updates when no host updater reports.

### Fixed

- Usage detects Claude Code and Codex started through a wrapper shell, idle Claude Code 2.1.330 sessions show Idle, and Claude Code sessions on Windows are classified.
- `install.sh --service` and `--root` on a host without systemd (for example a container) stop before changing anything and point to "Run as me".

## v0.19.0

### Added

- Presence records: a running Worker publishes a small record (`minion.presence/1`) that other programs can read to see whether it runs for a user or as root, and its version. User Workers write privately (0600); a root Worker writes world-readable files in `/run/minion-presence` (`/var/run/minion-presence` on macOS). See [Read the Worker's status from another program](docs/guides/integrate-local.md).
- Nominated usage read-back: users nominate a session and a provider token bucket, administrators a Hub-wide pair; the Hub copies the published meters (percent, amounts, `ms` durations, headline, expiry, coverage) to Workers, where programs can draw a progress bar. New statistics appear as new meters. See [Usage and subscription limits](docs/guides/usage.md#show-usage-on-your-workers-nominations) and [security](docs/security.md#the-public-presence-files-of-a-root-worker).
- A discovery command that reads and verifies the presence records of the account and of root (`minion worker discover [--json] [--usage] [--state-dir DIR]`, JSON schema `minion.worker-discovery/1`, exit codes 0 running, 1 none running, 2 search failed). `minion worker status --json` gains an optional `usage` field. The Hub delivers nominated usage to Workers over a Hub-only control frame (`readback.apply`), acknowledged after local publication, retried with backoff and reconciled every 60 seconds, with a 5 minute lease; it is cleared when a Worker is revoked, reset, replaced or changes owner.

## v0.18.0

Released 2026-11-21.

### Added

- Prepared Claude Code sessions can receive generation-bound Minion Master tools only while nominated. The first qualified attachment is Claude Code 2.1.330 on Linux tmux with the required kernel peer-lifetime capability. Revocation withdraws the current catalog and denies stale calls at the Hub; historical native declarations can remain. Unprepared running sessions and unqualified harness/platform combinations report their limitation instead of changing shared profiles or restarting coding sessions. See [AI agents](docs/guides/agents.md).
- The Hub's Usage menu shows authorized session statistics, native account limits, per-account collectors, historical snapshots and private token buckets. CLI and Hub-connected MCP clients can read the same scoped data. See [Usage and subscription limits](docs/guides/usage.md).
- Metadata-capable Workers disclose collection, including unshared session metadata, at enrollment. The independent console sharing and control rules still apply. Existing enrolled Workers can review the policy before enabling the new reports.

### Fixed

- Syslog reconnects honor backoff after every failed write, including a failed retry.

## v0.17.0

Released 2026-11-21.

### Added

- **Android Worker app:** a signed sideloadable APK includes both armeabi-v7a and arm64-v8a native Workers, supervised private tmux, foreground-service status, structured pairing, TV launcher and D-pad support. The release certificate is stable across updates; the APK is covered by the signed release checksums. Device-specific ARM/TV/root validation remains required; root must be granted externally.

- **Hub updater for TrueNAS (host executor).** `minion hub-updater` (Linux) runs as root on the NAS from a TrueNAS Cron Task and updates a Hub that runs as a custom app: it verifies the release with the signed manifest, pins `repository@digest` through the middleware (the compose document goes over stdin, never argv), stops the app, snapshots the Hub dataset, waits for sustained readiness of the candidate image and, on failure, restores in the order stop, `zfs rollback`, previous compose, start, then quarantines the digest. A journal reconciles a killed run first on every tick, also outside the window. The candidate runs read-only (`MINION_MAINTENANCE=readonly`, new: the Hub answers 503 to anything that could change state) until it has proved itself, and the restore pins the verified previous digest. Administration > Updates displays the host status, saves a browser-admin-only opt-in schedule, and queues an exact expiring update through bounded request/status files. Trial startup serves health probes only; the executor records a durable forward-only decision before normal traffic starts, so later recovery cannot erase acknowledged writes. See [Hub updater for TrueNAS](docs/install/hub-truenas-updater.md).
- **One simple command to add a Linux or macOS Worker.** The Add worker result starts with `curl -fsSL <release>/install.sh | MINION_JOIN='mnj1-…' sh -s -- --join` (the versioned installer of the Hub's own release over HTTPS; the token is in the environment of `sh` only) and the same without the token, which asks for it at a masked prompt on `/dev/tty`. A folded "Verified variant" downloads the installer to a private file and runs it only if its SHA-256 equals the digest in the signed release manifest of that tag (the Hub verifies the manifest's signature, like `minion update`). Both pass the Worker's name (`--name`) and say that all tmux sessions are shared (`--share none|later` keeps them private); the prompted one clears an exported `MINION_JOIN`. The verified variant also gives `install.sh` the SHA-256 of the signed `checksums.txt`, so the archive is authenticated too, and its exit status is the installer's. They are offered only when the Hub's release has an `install.sh` with `--join` (v0.17.0 and later; a development build shows the manual commands), never contact GitHub while the update check is off, and the digest lookup runs in the background (the page never waits for it). `install.sh` refuses to run unless its last line was reached, and says "finished" when a join completes.
- **`install.sh --join [--share all|none|later]`** installs the binary (verified as before) and then runs the installed `minion` by absolute path as `worker up --join - --interactive-replace`, with the token on standard input of that one process (from `MINION_JOIN`, or a masked prompt on `/dev/tty`). It needs tmux and refuses to run as root. Without `--join` the installer behaves as before. The script now runs from its last line, so a cut-short download runs nothing.
- **`minion worker up --join` on a machine that already has a Worker:** with `--interactive-replace` (used by `install.sh`), `--replace` or `--keep` it first looks at the target read-only (enrolled, configured, paused or running), prints the account, state path, name, Hub address and Hub key words, and asks on the terminal "Replace it with <new Hub>? [y/N]". No (the default) changes nothing and exits 0. `--replace` and `--keep` answer for scripts; with neither and without a terminal the command refuses and names them. Replacing is a transaction: the old Worker is stopped and a private copy of its profile is kept, the profile is cleared of all trust settings (pairing, share choice, Hub settings including `hub_fingerprint`, and the pause marker), the join runs, and the copy is deleted only when the new Worker is live (waiting for approval counts). On any failure the old profile is restored and the old Worker started again. Callers that give none of the flags, such as the tray, keep their behaviour.

### Changed

- **Long commands are folded** on Add worker and Invitations: a keyboard-operable disclosure shows the first three lines, with a Copy button that stays visible and copies the whole text. If the browser refuses the clipboard, the disclosure opens and the whole text is selected. Tokens and fingerprints are never cut, and the token-bearing result is no longer a live region (a short status line announces it instead).
- **A join command with the token inside needs a key that expires.** Add worker keys are one-use; an administrator who picks "Unlimited" now gets only the token and the masked command (no keyed command blocks).
- **Android Worker, Phase 1 (Go and native binaries).** The Worker runs as a managed host on Android: the settings `tmux_binary` (the absolute tmux executable every tmux call uses: run, status, setup and the local commands), `managed_host` (`android`) and `tmux_config` (the fixed tmux configuration, default `/dev/null`). In the Android mode every tmux call passes `-N` and `-f <fixed config>` (no command starts a server that nobody supervises), the private `tmux_socket` is required, the sessions stay on the `tmux` wire kind with the fixed reason `android_managed` and are reported as hidden (the Hub policy for hidden sessions applies; the refusal message names the Android app), the pairing code is shown only through the owner-checked local status (`pairing`: code, expiry, the Worker words and the Hub words as presented; never on the console or in the log), and binary self-update (`minion update`, the Hub update) is refused. The three settings are given by the supervisor in the environment at every start and are never saved to the config file.
- **Interrupted join recovery.** A join that was killed between the enrollment commit and the save of the Hub address now finishes: the next `worker up` saves the Hub address and the checked fingerprint from the committed enrollment, and a credential-free `worker run` connects to the Hub of the enrollment instead of staying in Solo mode.
- `worker up --configure-only --non-interactive-share <choice> --join -` works on a fresh state with pipes only: the first-run gate checks the managed tmux binary, never the one on PATH.
- **Native build for Android** (`scripts/android/build-native.sh`, `android/native/versions.lock`, the reusable `.github/workflows/android-native.yml` that `ci.yml` calls): the cgo Worker (`CGO_ENABLED=1`, NDK r28c clang, cgo resolver), tmux 3.8 with libevent and ncurses linked in, and a terminfo database, for arm64-v8a and armeabi-v7a at API 29, with sources pinned by SHA-256 and the ELF headers checked (ABI, PIE, `/system/bin/linker(64)`, DT_NEEDED closure of Android system libraries, 16 KiB LOAD alignment).

### Fixed

- The release resolves the image digest once and signs, verifies and publishes that exact digest; the pre-publish check fails if the signed manifest names another digest than the smoke-tested one.
- Worker connection shutdown no longer waits for a stalled local log during the online transition or handshake teardown.

- **Administration > Updates:** Check now shows the answer without a manual reload (it waits for the check, up to 20 seconds, then reloads the page; a slower check reloads the page every 5 seconds for at most 2 minutes).
- The startup summary names the run mode once (it said "background (background (shell login))").

## v0.16.0

Released 2026-11-20.

### Fixed

- **The Hub key words are always shown in full.** The top bar cut the six words after 34 characters (and hid them on phones), which defeated comparing them with a Worker. They are never shortened now and have their own row under the top bar; everywhere else (invitation and join results, popover, Workers list) the words wrap at their hyphens instead of scrolling sideways. A tray MISMATCH line also shows the words.
- **Workers list:** an offline Worker no longer shows a second "Offline" next to its Forget button; the buttons line up with the row.
- **Invitation keys and join commands**: after "Create the key" (and "Create the join command") the panel now starts with a note that the invitation is saved and listed until it expires, is used or is revoked, and that only the secret text cannot be shown again. A Done button at the top and the bottom goes back to the page, the new key shows in the list at once, and the key field is as wide as the panel so the whole key is visible.

### Changed

- **Copy buttons everywhere**: the join token and every join command on the Add worker page, and the Hub fingerprint in the header popover, now use the same copy icon as the rest of the Hub. The README and the install guides show one-line installs and standalone commands in code blocks, which GitHub gives a copy button.
- **Worker onboarding** starts the Worker in the background and prints its live state and identity words. On Linux without systemd, the Worker starts at shell login and supports `worker down` and `worker start`. Run `worker up` again to move a foreground Worker into the background. Use `--foreground` for an attached terminal.
- **Add worker** keeps the one-time credential out of background process arguments and generated Compose files. The native commands ask what to share on a terminal. Without a terminal the Hub commands share all sessions (`--non-interactive-share all`); the wizard default is all too. Under sudo you can choose the invoking user's sessions or root's.
- **Terminal dependencies** are checked before setup. The Unix installer installs missing tmux through the platform package manager, and the Windows installer installs missing psmux through WinGet. See [upgrading an old tmux](docs/install/worker-linux.md#upgrading-an-old-tmux) for distributions shipping a version below 3.2.

### Changed

- The Worker works with tmux 3.0 and newer (was 3.2), so the tmux of Ubuntu 20.04, Debian 11 and the Orange Pi images is enough. CI now also tests tmux 3.0a. With tmux older than 3.2 the Worker makes its tmux socket owner-only (older tmux creates it group-writable), refuses the keys C-2 and C-? (older tmux sends other bytes for them) and warns when tmux is older than 3.1c (CVE-2020-27347: install your distribution's security updates). On tmux 3.0 a session whose name contains a tab or newline is listed but cannot be opened.

## v0.15.0

Released 2026-11-20.

### Added

- **`minion worker start`**: starts the installed Worker through its manager (the Windows logon supervisor, the systemd unit, the launchd agent); a running Worker is left alone, and without an installation it points to `minion worker up`.
- **`minion worker pause` and `resume`**: one saved choice, until resumed. A pause closes the Hub link and gates reconnect, automatic pairing and key retirement before it reports success; local control and open terminal sessions keep working, a restart keeps the pause, only a running Worker pauses, and `worker status` shows it (`paused`).
- **`allow_new_sessions`** (`minion worker config set allow_new_sessions off`): a local block of new sessions for every origin (Hub, API keys, `mn`, MCP, local commands), enforced where sessions are created; the Hub cannot lift it. `worker status` shows `new_sessions` (`unknown` when a running Worker cannot confirm it). New error code `new_sessions_blocked` (and `paused`).
- **`minion worker up --join -`** reads the join token from standard input; nothing else changes (sharing as with `--join <token>`). `worker up` saves `allow_new_sessions` only when given by flag or environment. See [Control a Worker from the command line](docs/guides/worker-control.md).
- **Desktop tray `minion-tray`** (Windows, Ubuntu desktop, macOS Intel and Apple Silicon), a separate opt-in download built by the `tray` workflow: status, Start, Stop, Pause/Resume, Hub address and six key words with a prominent MISMATCH, Hub setup (token on stdin), session picker, new-session block. Every item runs a `minion` command; see [Desktop tray](docs/guides/tray.md). `minion`, its packages, the image and the armv7 build gain no GUI dependency; `make lint` checks it (`scripts/check-headless.sh`).
- **Compliance monitoring** (`compliance_monitoring` = `off`, `actions`, `output`, `full`; Hub, live, in the Log export settings, off by default). Sends what happens in sessions through the Hub to the OpenTelemetry log sink as `minion.session.action`, `.output` and `.input` events: who did which operation, the screens and received pipe turns that were returned, and at `full` the text, keys, answers, tasks and messages that were sent. Needs the otel sink on with an https (or loopback http) endpoint and follows `otel_level`; Minion's secret formats are masked before long text is split into chunks. Session pages show a banner and `minion` prints a line to standard error while it is on. Local typing and unread screens are not captured; text sent through Minion is recorded as sent. See [Compliance monitoring](docs/guides/observability.md#compliance-monitoring).

## v0.14.0

Released 2026-11-19.

**Windows: upgrade with care.** Releases up to 0.13.1 installed the Windows Worker as a LocalSystem service that ran a `minion.exe` your programs can change. From this release the Worker runs as you, started at logon; remove the old service first as described in [the Windows guide](docs/install/worker-windows-preview.md#move-from-the-localsystem-service).

### Added

- **Autoscroll on every session view** (full page, the side panel, the panes): a box next to "Input goes to …" (or in the screen bar for people who can only view), on by default. On, the newest line of the screen stays in view after every update; off, the screen keeps the position you scrolled to. The choice is remembered in this browser.
- **New session and Forget in one row.** On the Worker page and in the Actions column of the Workers list, the buttons sit side by side; Forget asks for confirmation (with what forgetting does) before it removes the Worker.
- **Worker traffic log** (`worker_traffic_log=metadata`, Hub and Worker, set independently): one record per frame between the Hub and its Workers on the op, ca and ev streams, with direction, Worker, stream, frame type, op, request id, size and, on a result, its duration and outcome; never a body. To `local`, `syslog` and/or `otel` (`worker_traffic_log_to`) at a fixed info level, never the in-memory log; the Hub can narrow it to some Workers (`worker_traffic_log_workers`) and switches it live. See [Worker traffic log](docs/guides/observability.md#worker-traffic-log).
- **Worker traffic metrics** (`otel_metrics` = `off`, `basic`, `detailed`, Hub, live) over OTLP/HTTP to the OpenTelemetry endpoint: connections, Workers online, WebSocket payload bytes (L1) and application frame bytes and counts (L3) by direction and stream, ops, calls, stream admission and pipe turns; `detailed` adds per-Worker series (first 500 Workers, then an overflow series) and connection and op durations. See [Worker traffic metrics](docs/guides/observability.md#worker-traffic-metrics).
- **Log export to syslog and OpenTelemetry** (Hub and Worker). Each sink has its own switch, endpoint and level (`syslog`, `syslog_endpoint`, `syslog_level`; `otel`, `otel_endpoint`, `otel_level`), independent of `log_level`. Syslog: RFC 5424 (facility local0, UTF-8 BOM, the MSG is one JSON object within `syslog_max_bytes`, default 2048) over `udp://`, `tcp://` or `tls://` with octet counting, CA file and optional client certificate (mutual TLS). OpenTelemetry: OTLP over HTTP (protobuf, gzip) to `<otel_endpoint>/v1/logs`, or the standard `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` / `OTEL_EXPORTER_OTLP_ENDPOINT`; headers from the Hub secret `otel_headers` or the Worker file `otel_headers_path` (`OTEL_EXPORTER_OTLP_HEADERS` is ignored with a warning). Logging never waits for a sink (bounded queues, 5 s deadlines, reconnect with backoff); sink problems are reported on standard error only, once a minute. Guide: [log export](docs/guides/observability.md), collector example `docs/examples/otelcol-minion.yaml`.
- **What leaves the machine is masked first.** The generated initial admin password and the pairing code stay on the console (same level and text as before) and never reach a sink; secret attribute names, URL credentials and queries, pairing codes and Minion's own API keys (`mn_`), invitation keys (`mnk_`) and join tokens (`mnj1-`) are masked in messages and attributes, values are capped in size. The in-memory Worker log (`worker_logs`) now also masks `mn_`, `mnk_` and `mnj1-` secrets.
- **Hub settings: Log export section saved as one change**, checked as a whole before it is stored and applied at once without a restart (the old sink's queue is dropped, so nothing queued for an old endpoint or credential is sent with a new one). `settings_set` accepts `changes` (several keys, all or nothing, one transaction with its audit rows); a refused combination stores nothing. `hub_diagnostics` reports `sinks` (destination, level, sent, queued, dropped by reason, last error); `minion worker status` shows them too (`log_sinks` in `--json`).
- **Safe live changes:** `settings_list` reports a `revision` per setting and a change may name it; a settings page loaded before someone else's change is refused instead of undoing it. A configuration that cannot be applied switches both sinks off and the status says why. A batch with a secret value is read from standard input only (`--changes -`). OTLP redirects are not followed and collector error texts are never shown or logged. Administration > Diagnostics shows the sinks.
- **Worker:** the sink settings are flags (`--syslog`, `--syslog-endpoint`, `--otel-headers-path`, ...), variables and config-file keys; `worker up` checks them as the service will see them before it saves anything, saves relative file paths absolute, and the service uses them. An enrolled Worker exports with its Worker id as `service.instance.id` and `minion.worker.id`. A sink setting that does not validate never stops a Worker or the Hub: both sinks stay off and the status says why. Certificate and key paths use `MINION_..._PATH` variables (for example `MINION_SYSLOG_CA_PATH`).

### Changed

- **The Hub's `log_level` from Administration > Hub settings now applies, at once.** Before v0.14.0 a `log_level` stored in the panel did nothing; the Hub read only `MINION_LOG_LEVEL` and `-v`. The order is `MINION_LOG_LEVEL`, then the panel, then `-v` / `MINION_VERBOSE`, then `info`.
- Dependencies: `go.opentelemetry.io/otel/sdk/metric` v1.47.0 and `otlpmetrichttp` v1.47.0 (traffic metrics); `go.opentelemetry.io/otel/sdk/log` v1.47.0, `otlploghttp` v0.23.0, `contrib/bridges/otelslog` v0.21.0, `otel/sdk` v1.47.0 (only in `internal/obs`; no gRPC).

### Security

- **The Windows Worker runs as you, never as LocalSystem.** Up to 0.13.1, `minion worker up` (elevated) installed the Worker as the LocalSystem service `MinionWorker`, which ran the `minion.exe` from the user's profile (`%LOCALAPPDATA%\Programs\minion`, where `install.ps1` puts it): any program running as that user could replace the file and run code as SYSTEM at the next service start. Now `minion worker up` on Windows, in a normal terminal and without an administrator, writes the value `MinionWorker` into the user's own Run key (`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`) and starts the Worker: at each logon the Run entry starts `minion worker launch`, a supervisor that runs `minion worker run` as the user without a console window, starts it again after a failure (5 s, doubling up to a minute) and at once after a self-update (exit 75), and ends with `minion worker down`. The Worker serves the user's psmux sessions; its output goes to `worker-launch.log` in the state directory. `minion worker up` refuses an elevated prompt (the Worker would run with administrator rights). An existing LocalSystem service is reported by `minion worker status`, refused by `minion worker up` (exit 21, with Windows' own commands: disable the service, stop it, check it stopped, `sc.exe delete`, check it is gone) and, only on an account that is always administrator (no separate elevated prompt), removed by `minion worker up` with the same steps (prefer the Windows commands: they never run the profile's `minion.exe` with administrator rights); started, the old service now ends at once instead of running the Worker as SYSTEM. The LocalSystem install path is gone. See [moving from the LocalSystem service](docs/install/worker-windows-preview.md#move-from-the-localsystem-service).

## v0.13.1

Released 2026-11-18.

### Added

- **Check now on Administration > Updates.** The button starts a check of GitHub at once and the page shows "checking GitHub now…" until the answer is in (at most one call a minute; a sooner press shows the last answer). The page also shows when the check last ran and why it failed, if it did. New admin operation `hub_update_refresh` (POST only); `hub_update_check` also reports `checked_at`, `checked_channel` and `check_error`.

- **The Hub's identity in view everywhere.** Every signed-in page shows the Hub's fingerprint words in the top bar, left of the theme switch; a click or tap opens the words and the 52 characters (on narrow screens the button shows only the icon). `minion worker up`, `down`, `pair`, `join` and `reset` first print the words of the Hub key the Worker pinned, for example `Hub key: word-word-word-word-word-word (compare with the Hub's top bar)`, and MISMATCH when the Worker refuses the Hub's key.

### Fixed

- **Check now never makes a request or a Hub stop wait for GitHub**: the check runs in the background and ends with the Hub.
- **A changed `update_channel` is checked at once.** Before, the Hub kept the answer of the old channel until the next daily check, and the Updates page showed the new channel next to it, for example "latest … v0.11.0 (up to date)" while v0.13.0 was out. The Hub now notices a change of `update_channel` or `update_check` within 15 seconds and checks again (at most one GitHub call a minute, so within about a minute); until then the page says it is checking the new channel and offers neither the old answer nor an update to it.

## v0.13.0

Released 2026-11-18.

### Changed

- **New look of the Hub web UI ("Console").** Light and dark themes with an emerald accent for primary actions and live states, status colours by meaning (green online/live, amber working or waiting, red errors, grey idle or offline), a distinct icon per navigation item, sidebar groups, a sticky top bar, a mobile layout, and a new logo and favicon. The theme switch is three icon buttons (System, Light, Dark) instead of a list. Every menu item, page, form and action of v0.12.0 is kept; the checklist used to verify this is [the Hub UI map](docs/architecture/hub-ui-map.md).
- **Easier to use, all in the browser:** a page and section search in the top bar; sortable table headers that keep their order across live updates (also remembered per tab); a click anywhere on a Worker row opens the Worker (Ctrl/Cmd or middle click: new tab); help popovers next to headings and fields; copy buttons beside commands, fingerprints, keys and URLs; local filters with removable chips on Users, API keys and Invitations; relative times with the exact time on hover; Hub settings mark unsaved rows, offer Reset and warn before leaving with unsaved changes; password show/hide and a match hint; busy labels on buttons while a request runs; empty states that name the next step. Audit sorts the rows of the current page and its date fields use date pickers (same field names and formats).
- **Removing a stored secret in Hub settings asks first** (for example the OIDC client secret); Save and Remove of the same secret cannot run at once.

## v0.12.0

Released 2026-11-18.

### Added

- **Add a Worker in one step.** The Add worker page replaces the panel "Give the Worker this Hub's fingerprint" with a form (optional name, validity, 1 hour by default) that creates a one-time invitation key and shows, once, a **join token** (`mnj1-...`) and the ready commands: `minion worker up --join <token>` (Linux, macOS), `minion.exe worker up --foreground --join <token>` (Windows), a Linux system-service variant that passes the token to sudo through a private file (`MINION_JOIN_FILE`), one `docker run` with `MINION_JOIN` and the Hub's own release, a Compose project, and the same with separate `--server`, `--hub-fingerprint` and `--invite-key` for older Workers. The token holds the Hub URL, the full Hub fingerprint and the one-time key (base64url of a canonical JSON payload); the Worker connects only to the Hub with that fingerprint and enrolls with the key, which works once and expires. Blueprints: the Docker Swarm join token and `kubeadm join --discovery-token-ca-cert-hash`; no new cryptography. The fingerprint panel for pairing with a code moved into the same section. See [add a Worker in one step](docs/guides/pairing.md#add-a-worker-in-one-step).
- **Worker setting `join`** (`--join` on `worker up` and `worker run`, `MINION_JOIN`, `MINION_JOIN_FILE`; never the config file). It sets `server`, `hub_fingerprint` and `invite_key`; a different value of one of them given by flag or environment (an empty `--server` included), or saved in the Worker's config file, is an error that names both sources, before anything is sent or written. A token that is given but empty is an error. The token is a secret: never saved, printed or logged; on the command line the Worker warns that it is visible in the process list. `minion worker join` and `pair` refuse a join token with a pointer to `worker up --join`.
- **Pairing requests** (like NetBird's peer approval). A Worker that waits with a pairing code appears under **Pairing requests** on the Workers page and on Add worker: announced name, source address, waiting since, the Worker fingerprint (words and 52 characters) and the public pairing id. An administrator admits it with one click after checking that the Worker's console shows the same Worker fingerprint and this Hub's words (the dialog shows both); the Hub enrolls exactly the key the request presents and the Worker joins the administrator's space approved. **Reject** removes the request; the Worker shows that it was rejected and a new code, and the same key is not listed again for 10 minutes. New admin-only operations `pairing_requests_list`, `pairing_request_admit`, `pairing_request_reject` (API, MCP, audit `worker.pair_admit`, `pairing.reject`). A Worker offers a request only with `hub_fingerprint` set or a Hub TLS certificate its computer trusts for the Hub's host name on the connection that carried the upgrade (no redirect, no HTTP proxy); otherwise only its code pairs. Each request shows, and its admission must name, the Hub key its handshake was answered with; a Worker that comes back to a committed pairing is not listed (it completes with its owner's code). Guide: [pairing](docs/guides/pairing.md#admit-a-waiting-worker-pairing-requests), guarantees: [security](docs/security.md#pairing-requests).

### Changed

- **Every invitation command pins the Hub.** The commands shown for a new invitation key add `--hub-fingerprint <52 characters>` (the fingerprint in the key), and the Compose project sets `MINION_HUB_FINGERPRINT`. The Compose projects also pass `MINION_SHARE_EXISTING` (give it on the first start: `MINION_SHARE_EXISTING=later docker compose up -d`), which a container Worker needs to start.
- **Compatibility:** `--join` and `MINION_JOIN` need Worker v0.12.0; an older Worker refuses `--join` as an unknown flag and ignores `MINION_JOIN`. The page shows the separate flags for older Workers (v0.11.0 or later for `--hub-fingerprint`).
- **The code box shows the Hub that answered.** Without `hub_fingerprint`, the box shows "Hub fingerprint: <six words>" of the Hub that answered, "confirmed by the pairing code: the Hub shows the same words on Workers > Add worker", and the Worker fingerprint; the old "NOT CHECKED ... stop, and start again with --hub-fingerprint" text and the matching log warning are gone. The code confirms the Hub: the CPace session id is the hash of both hellos, which carry the Hub's key. The paired box says "Confirmed by the pairing code" (or "Admitted on the Hub without the code"). The Hub check stays `not verified` in the status and on the Hub, as before. With `hub_fingerprint` set the box is unchanged.
- **The Hub sends its hello at once in pair_code mode** (before the code is claimed), so that a waiting Worker can show its words; the Worker's first CPace message follows and the Hub waits for the claim or the admission. Wire protocol 3.0 is otherwise unchanged: one new frame `pair.admit` and one new hello feature `pair.admit`, used only between a 0.12 Hub and a 0.12 Worker. A 0.11 Worker pairs with a 0.12 Hub by code as before (it is not listed as a request); a 0.12 Worker pairs with a 0.11 Hub by code, and its box then has no Hub line.

### Fixed

- **A Hub-triggered update is no longer rolled back because you restarted the Worker by hand.** The update crash guard counted every start of the new binary that had not been committed yet (5 minutes with a live Hub connection) as a failed start; three of them within 10 minutes restored the previous binary. While the Hub was unreachable, or after the Hub removed the Worker, a few Ctrl+C restarts were enough to roll back a working release. The guard now counts starts like systemd-boot and Android A/B updates: a run that stops cleanly (Ctrl+C, `minion worker down`, a service stop, also while it is still starting) takes its start back once every module stopped without an error, so only starts that crash, fail or hang count. An update commits only after the Worker started every module. A failed install of a newer update puts the guard of the running, uncommitted update back, and a removal of the pairing during the trial (a signed revoke of the Hub, a reset) is recorded so that a rollback never restores it. A Worker that is not enrolled (removed by the Hub, or waiting for a pairing) commits the update at once, like a quarantined one: it works as intended, and a rollback would restore the pre-migration database and lose a pairing made meanwhile. An unreachable Hub still does not commit. The rollback log line now says why ("the updated Worker failed to start (3 starts within 10m0s that neither stopped cleanly nor stayed connected to the Hub for 5m0s); restored the previous binary").
- **Windows: no Worker left running behind the prompt after a rollback.** A foreground Worker that rolled back at start-up started the restored binary detached and exited: the shell showed its prompt while that Worker kept running, Ctrl+C no longer stopped it, and the next `minion worker up` failed with "the Worker state is in use by another process ... worker.lock". The restored binary now runs in the same console and the first process waits for it (the state lock is released first) and exits with its exit code; Ctrl+C stops the running Worker, and a Ctrl+C during the restore ends the process without starting the restored binary. Linux and macOS already replace the process in place.
- **A new code after an expired or cancelled one** (A40). When the Hub ended the wait because the code expired, the Worker kept the old code until its own timer ran out and only logged "the pairing code expired; the Worker shows a new one"; it now shows the new code at once, in one box with the reason. The reason of a new code is no longer lost when the next attempt cannot reach the Hub, and a start after an interrupted pairing whose code expired says "The code of the interrupted pairing expired. Use this new code." in the box of the new code.


## v0.11.0

Released 2026-11-18.

### Added

- **Hub fingerprint as six words.** The Hub and Worker fingerprints are also shown as six words from the BIP39 English word list (not a BIP39 mnemonic; the list is embedded, MIT licence) for the first 66 bits of the fingerprint, next to the 52 characters. Input takes six words (separated by `-`, spaces or commas, any case, the first four letters of each word are enough) or the 52 characters; all six words are always compared. Copy-ready values use the 52 characters.
- **Give the Worker the Hub it may pair with.** Worker setting `hub_fingerprint` (`--hub-fingerprint`, `MINION_HUB_FINGERPRINT`, config file). `minion worker pair`, `join`, `setup`, the `up` wizard and `worker up` with the sharing choice given (`--share-existing ... --server ...`, first pairing) in a terminal without it ask "NEW HUB - ENTER ITS FINGERPRINT" before any network check; `skip` pairs without the check. The Worker compares the Hub's root key before the pairing code is used: another Hub gets "HUB FINGERPRINT DOES NOT MATCH - NOT PAIRED" and nothing is stored. `join` and the wizard save the fingerprint with the address. An unattended Worker without it still pairs on first use, with a warning, and is shown as "not verified".
- **`minion worker verify-hub <fingerprint>`** checks a paired Worker against the value from the Hub host: a match is recorded; a mismatch is stored as a quarantine: the Worker does not connect, also after restarts, until `verify-hub <52 characters>` of the paired key or `--forget` (the setting `hub_fingerprint` never ends it). The command reports that a running Worker disconnected only once it confirmed it, and changes nothing while an older Worker (before 0.11) runs on the state directory (stop it with `minion worker down`). `--forget --pinned <52 characters>` removes the pairing (like `ssh-keygen -R`), removes `invite_key` from the config file and records the removal, so a start does not enroll again from an invitation key that is still configured. On a paired Worker `hub_fingerprint` is compared at every start in the same way; a value that names the key the Worker was checked with, or any key it moved through since by verified signed master-key rotations, connects with a warning to update it.
- **`minion worker status`** shows the Hub fingerprint (words and full), the **Hub check** (`not verified`, `matched the fingerprint you gave`, `from the invitation key`, or `DOES NOT MATCH ...`), the Hub identity alarm, the TLS path and the Worker fingerprint (words and full); `--json` has the same fields.
- **TLS path** (information only): the Worker reports whether the certificate it saw has the key the Hub serves (`keys_match`), another TLS endpoint is in between (`keys_differ`), the Hub received plain HTTP (`plain_to_hub`), the Hub does not report it or states no key for the connection (`unknown`; a malformed statement never fails the connection) or the connection is plain (`none`), with the certificate's name, issuer and whether this computer trusts it. Log line `hub connection` (`tls_path`) and a warning when the path changes.
- **`minion hub identity --words`** prints the words, the 52 characters, ready lines for a Worker (`minion worker join ... --hub-fingerprint ...`, `MINION_HUB_FINGERPRINT=...`, `minion worker verify-hub ...`) and the Hub's `TLS key pin`. The Hub logs `hub identity` with `fingerprint_words` and `fingerprint` at start, after every activation and daily. The web UI shows the words and the 52 characters on the identity page, Add worker, a created invitation key, a pending Worker and the approve dialog. `minion hub worker approve` accepts six words (pending Workers only).
- **CLI and MCP: TLS key pin.** A certificate error names the real reason and prints the presented key pin. New setting `hub_tls_pin` (`--hub-tls-pin`, `MINION_HUB_TLS_PIN`; one or more `sha256//<base64>`, separated by `;`) replaces the certificate check; a different key is a hard error. In a terminal the CLI may offer to trust the key: type the exact pin to save it for that Hub's origin (`hub_tls_pins`, one entry per `scheme://host:port`; a pin saved for one Hub never replaces or serves another, and a malformed entry is an error). With a pin or a CA file set, a plain `http://` Hub URL is refused (also loopback).
- Guides: [pairing](docs/guides/pairing.md#give-the-worker-the-hub-fingerprint), [when you do not want to rely on the proxy](docs/guides/pairing.md#when-you-do-not-want-to-rely-on-the-proxy), [what is protected](docs/security.md#trust-without-certificates).

### Changed

- **A paired Worker whose Hub identity changes** (another key, or a master-key rotation record that does not verify) keeps its pairing, keeps retrying, does not connect, prints "HUB IDENTITY CHANGED - NOT CONNECTING" once and shows the alarm in `minion worker status`.
- **Invitation keys and `hub_fingerprint`** must name the same Hub when both are given; otherwise the Worker stops before anything is sent. An invitation key protects against a proxy only when it reaches the Worker without passing through a page the proxy can rewrite.
- **The paired box** shows the owner first ("Not your account? Run: minion worker reset"), then the Hub fingerprint, whether it matched, the TLS path and the Worker fingerprint.
- **Compatibility:** wire protocol 3.0 is unchanged (one optional welcome member, `hub_tls`); the Worker database gets migrations 0010 and 0011. The Worker states its Hub check in the hello facts; the Hub keeps the last report with the Worker (Hub migration 0027), and its approval page shows "not verified" for a Worker that did not check the Hub, also while it is offline, and "unknown" for one that has not reported yet. A quarantined Worker, or one whose pairing was removed with `--forget`, counts as a good self-update: the crash guard never rolls it back to a release that cannot enforce that. The Hub's TLS listener no longer offers TLS session resumption (the served key must be known per connection). A 0.11 Worker works with a 0.10 or 0.9 Hub (TLS path `unknown`; use the 52 characters of `minion hub identity`, which has no `--words` there). An older Worker refuses `--hub-fingerprint` as an unknown flag and ignores `MINION_HUB_FINGERPRINT` silently. **Upgrade the Hub first.** Then run `minion hub identity --words` on the Hub host and `minion worker verify-hub <52 characters>` on each Worker, or add one `MINION_HUB_FINGERPRINT` line per Compose file.
- No new cryptography. The handshake still awaits an independent cryptographic review.

## v0.10.0

Released 2026-11-17.

### Added

- **No more hidden sessions by surprise.** A Worker reports what hosts its sessions (tmux, psmux or ConPTY) and whether a new session would be hidden from you, that is, not visible in your own `tmux ls` / `psmux ls` (Windows ConPTY inside the Worker, a private tmux socket, root's tmux server under a service manager), with the reason (a fixed reason code and text: no paths, user names or program output leave the Worker before it has checked the Hub). The Workers list and the Worker page show it ("Sessions: psmux (visible)", "ConPTY inside the Worker (hidden): psmux is not installed (winget install psmux)").
- **Hub setting `hidden_sessions`** (`MINION_HIDDEN_SESSIONS`, live): `user-controlled` (default), `allowed` or `disabled`. With `user-controlled` each user allows hidden sessions in **Account > Sessions** (off by default). The choice is account-wide, so `account_prefs_set` takes the person's own signed-in browser session or an API key that holds manage; an administrator acting as someone cannot change it (`account_prefs` reads it with any key). A refused `session_create` names the Worker, the reason and the exact fix; the New session buttons are shown disabled with the same text. The refusal is decided on the facts of the very connection that would get the request. Workers older than v0.10.0 do not report their host: on Windows they count as hidden (they may use ConPTY) and the refusal says to update them; on Linux and macOS they are not refused. Workers older than v0.9.0 report neither host nor OS, and nothing in their hello tells Windows from Linux: during a mixed-version rollout their session host is unknown, which counts as hidden when `hidden_sessions` is `disabled` and is allowed otherwise (so that a Hub-only upgrade does not block every old Linux Worker); their Worker page shows "session host unknown (update the Worker)".
- **`minion worker status`** shows how the running Worker runs (foreground console, Windows service, systemd or launchd service), its session host with the visible/hidden reason, and why it cannot list its sessions when that fails.
- **Windows: a foreground Worker warns about the Windows service.** When `MinionWorker` is also installed and running, or set to start at boot, `minion worker up --foreground` says that it serves only hidden ConPTY sessions and keeps a Worker connected after the console closes, with service-specific commands in an elevated PowerShell (`sc.exe stop MinionWorker`, then `sc.exe delete MinionWorker`; a stopped service that starts at boot: `sc.exe delete MinionWorker` or `sc.exe config MinionWorker start= demand`). The check only queries the service control manager, with the status and the configuration read separately.
- **The Worker page explains an empty session list**: "Waiting for the first session list", "Listing the sessions failed (as of the last connection)" with a fixed reason (the Worker reports the state of its listing when it connects) or "No visible sessions on this worker".
- **Worker setting `sessions_visible`** (`auto`, `yes`, `no`; Linux and macOS): says whether new tmux sessions are visible to the person. The Docker Compose example sets `yes` while it mounts the host user's tmux socket; a self-contained container Worker uses `no` (`auto` treats only the tmux server inside the Minion image as hidden: a `minion` installed by hand in a container you work in, such as a Coder workspace or a dev container, serves your own tmux server like on a host). With `auto`, a container Worker on the documented host-socket mount `/tmux/default` counts as visible, so a v0.9.3 Compose file without the setting keeps working after the update. Under a service manager `auto` judges the default tmux socket as seen by the service (the reason says so); when your shell uses another `TMUX_TMPDIR` or socket, set `tmux_socket` and `sessions_visible` (see the Linux install guide).
- **Worker debug logging**: `minion worker up --debug` (also `worker run`, or `MINION_DEBUG=1`) logs at debug level, with new debug lines for the session host choice (psmux path and where it was found, version, why ConPTY), every session listing whose result changed (count, sessions psmux cannot address), the share filter, the event stream, self-update steps and the restart path, the service detection, and the Hub connection (URL, proxy, TLS peer certificate subject, issuer and SHA-256 fingerprint, protocol).
- **Remote debug** (`--allow-debug-remote`, `allow_debug_remote`, `MINION_ALLOW_DEBUG_REMOTE`; off by default): the Worker keeps its recent log lines in memory, and its owner or an administrator can switch debug logging on for 1 to 1440 minutes (it switches itself off) and read the last lines through the Hub: the **Debug** panel on the Worker page, or the operations `worker_debug` and `worker_logs`. Every call is audited. A Worker that does not allow it refuses with the command to allow it. After an owner change the Hub sends `worker_debug` and `worker_logs` only to a connection welcomed under the current owner (otherwise "the Worker is reconnecting after an owner change; try again", and the stale connection is closed), and the Worker clears its in-memory log when a welcome names another owner. Dial, download and update errors show every URL as scheme and host only, in the local log, the in-memory log and the update status (no user information, even with an unescaped `@`, no path, no query).

### Fixed

- **Windows self-update with psmux**: the "sessions would end on the restart" refusal now applies only to ConPTY sessions; psmux sessions survive a Worker restart and no longer block an update.

## v0.9.3

Released 2026-11-16.

### Added

- **`minion worker down`** stops the running Worker of this user, the opposite of `worker up`: a foreground Worker over its control socket, a service through its manager (systemd user or system unit, launchd, Windows service). When it needs rights it does not have, it prints the exact command (for example `sudo systemctl stop minion-worker@<user>.service`).

### Changed

- **One confirmation, on the screen**: a refused send (a shell, an open dialog, a busy agent) shows one "Send anyway" button in the result, no pop-up; after it, sending on that page goes without asking until you leave or reload it ("Allowed until you leave this page"). The "Send without asking" checkbox says when it is always allowed by this browser's setting.
- Messages that tell you to stop or reset a Worker print the full command (`minion worker down`, `minion worker reset`).

### Fixed

- **Self-update inside a container**: only a Worker that runs from the Minion image (`/ko-app/minion`, or `MINION_IN_CONTAINER=1`) is told to pull a new image; a binary installed by hand in any container updates itself like on a host.
- **Windows foreground self-update keeps the console**: the old process stops cleanly, then runs the new binary in the same console and waits for it, so Ctrl+C stops the Worker and `minion worker up --foreground` can start again (before, the new Worker ran on in the background and held the state lock).

## v0.9.2

Released 2026-11-16.

### Added

- **Forget a Worker** (Worker page → Forget this Worker, owner or administrator): removes any Worker, approved or not, online or offline. The Worker is told to forget this Hub (its pairing is wiped, its sessions keep running) and must be paired again to come back.
- **psmux on Windows**: a Windows Worker that runs as you (`minion worker up --foreground`) serves your psmux sessions (psmux 3.4.0 or later, tmux for Windows): they show up in the Hub, survive Worker restarts and updates, and stay attachable with `psmux attach`. Every pane is addressed through its session (psmux numbers panes per session), paste buffers go to that session's server, and a write is checked against the pane of the same session. `session_host` chooses `auto` (default: psmux when installed and the Worker is not a service), `psmux` or `conpty`. Plain PowerShell or cmd windows outside psmux stay invisible.
- **First run offers psmux**: a foreground Windows Worker in a console without psmux asks once whether to install it with `winget install psmux` (`psmux_offer`).

### Known limits

- **psmux**: session names that psmux cannot address literally (starting with `%`, `@`, `$` or `=`, or containing `:`, `.`, quotes, `#` or `;`) are not served, and a program argument that is a psmux command separator (`;`) is refused. On psmux the check that the pane still runs the reviewed program and the paste are two requests (psmux 3.4 runs `if-shell` that way), not one as with tmux: a program that exits in that moment can receive the text in the shell that follows it.

## v0.9.1

Released 2026-11-16.

### Added

- **Send without asking** (session page header and Worker page → Sending): text, keys and the task go out at once, also to a shell, an open dialog or a busy agent, without the "Send anyway" button and its second confirmation. Per session or for all sessions of a Worker (the Worker switch shows when only some sessions have it). Kept in this browser; a dialog answer is still checked.

### Changed

- **Minion control is easier to use**: "Add a master session" lists only the Worker's shared sessions, with a filter, and says so when there is none (no free-text name); the matrix is compact and labelled; "Who can control this session?" is one list of Worker / session and answers as soon as one is chosen.
- **Worker names start with their owner**: a new Worker is named `<owner>-<name>`, where `<owner>` is the owner's username without an e-mail domain, in lower case, with dots and underscores as dashes (`patrick.romatowski@example.com` → `patrick-romatowski-asus-w11`); the rest is the name the Worker asks for, its short host name by default. Existing Workers are renamed once by the upgrade (a rename that would collide is skipped), a rename keeps the prefix, and a reassigned Worker takes its new owner's prefix.
- **Session lists show `worker/session`**, so sessions with the same name on several Workers are told apart.

### Security

- **A session that ends or stops being shared loses its control settings**: when the Hub sees it end (or the Worker no longer lists it after a reconnect), its master nomination is deleted and it is taken out of session-limited acceptances and migrated limits; a limit that loses its last session allows nothing (it never falls back to the whole Worker or to "accepts all Workers"). A new terminal under an old name then starts as a slave session. Limit: a session that ends and comes back under the same name while the Hub is down is not detected (the Hub knows sessions by name only).

## v0.9.0

Released 2026-11-16.

### Added

- **Update Workers from the Hub**: the Workers page shows each Worker's release (`v0.8.2 → v0.9.0` when it is older than the Hub) and, for administrators, **Update** and **Update all outdated**. The Worker installs the Hub's exact release with the same signed, checksum-verified updater as `minion update` (no downgrade, protocol range checked), restarts by itself (systemd, launchd, the Windows service, or in place in the foreground) and keeps the previous binary: a new release that fails to start three times within 10 minutes is rolled back. A Worker installed by a package, in a container or with `remote_update off` says why it cannot update itself. Workers report their release, OS and architecture in the hello (additive). New ops `worker_update`, `worker_update_status` (admin only).
- **The Hub updates itself in Docker (opt-in)**: with the overlay `deploy/compose/hub.self-update.yaml` (Docker socket, channel tag `MINION_TAG`), **Administration > Updates** offers **Update the Hub to vY**. The Hub verifies the release, pulls the image by the digest recorded in the signed release manifest (new in 0.9.0: `image`), refuses when connected Workers would lose their protocol, and lets a helper container replace it: the data directory is copied first, the new container keeps the old settings and volumes, and a release that is not ready within two minutes is rolled back. Setting `self_update` (`auto`, `off`). New ops `hub_update_check`, `hub_update` (admin only; `hub_update` not with an API key).
- **Version in the left rail and a daily release check**: the bottom of the left rail shows the Hub's release; administrators see a second line `vX → vY` (linked to the release notes) when a newer release exists in the channel. The Hub checks GitHub once a day (`update_check`, on by default; `off` contacts nothing) and follows `update_channel`: `minor` (default, patches of the running X.Y), `major` or `latest`. The answer is kept in memory; pages never call GitHub.
- **Minion control: master and slave sessions, two-sided consent** (Access > Minion control, administrators): a session controls sessions on other Workers only when it is a nominated **master session** and the target Worker **accepts control from** its Worker (or from All Workers); every other session is a **slave session**, the default, and no Worker accepts anybody by default. **User-managed** (on by default) can be turned off per Worker so that people and API keys no longer reach its sessions. The screen saves every click at once with Undo, shows a matrix of who accepts whom, and answers "who can control this session?". 0.8 session-control grants are migrated unchanged (master sessions limited to their old targets); Administration > Session control leaves the navigation and `session_control_set` with targets is refused (use the new ops). Ops `control_list`, `control_set_worker`, `control_set_master`, `control_set_accept`, `control_check`. Guide: [Minion control](docs/guides/minion-control.md).
- **Minion for AI agents**: `minion --help` starts with a line for agents, and `minion llm-help` prints the operating guide (detect, install, pair, sessions, MCP, Minion master). The repository is a plugin marketplace for Claude Code (`/plugin marketplace add patrick-1984/ai-minion`, `/plugin install minion@minion`) and Codex (`codex plugin marketplace add patrick-1984/ai-minion`): the `minion` plugin brings the same guide as a skill and, in Claude Code, `/minion:setup` to install minion and pair a Worker with the user's consent. New guide: [AI agents](docs/guides/agents.md), with the **Minion master** (a session with a session-control grant; nothing is a master by default).
- **Thin Workers (preview)** in C for OpenWrt routers and ESP32 chips (`clients/`): they speak the Worker protocol 3.0, so this Hub serves them unchanged; enroll once with an invitation key, nothing on the device expires. OpenWrt packages of about 50 KB (shell sessions), ESP32 firmware with a command console (GPIO, ADC, PWM, I2C, Wi-Fi), a serial-console bridge and the device log. Released separately as prereleases `thin-v*` (`thin-v0.1.0-preview.1`); design in `docs/architecture/proposals/thin-workers.md`.
- **psmux evaluation (spike)**: a Windows CI job runs the tmux adapter against psmux v3.4.0 (sessions that survive a Worker restart work); psmux is not enabled yet: the blockers (pane ids per session, argv handling, key files) and the next steps are in [docs/architecture/proposals/psmux.md](docs/architecture/proposals/psmux.md). `version.CheckTrustedExecutable` checks that a helper executable is safe to run as a service.

### Changed

- **Sharing is easier with many sessions, and nothing is shared by default**: the Worker page's Sharing panel lists the sessions in a table with a **Shared** checkbox each, **All** and **None** buttons, (without JavaScript the panel works as before). The setup wizard's default is now "none" and its session list takes `all` and `none`; the install guides use `--share-existing none` and explain choosing in the Hub and `minion worker config unset share_existing`.
- **Hub settings in sections**: Administration → Hub settings groups the settings into sorted sections (Server and network, Sign-in and accounts, Single sign-on, Workers and invitations, Messages between sessions, Updates, Logging and troubleshooting, Storage) with jump links and a filter box; `settings_list` reports each setting's `section`.

### Fixed

- **Pairing is easy to follow on the Worker console**: the pairing code, where to enter it and the Worker fingerprint stand in a box of `=====` lines instead of between log lines; after pairing, and while the Worker waits for approval, a second box repeats the fingerprint, and the Hub's approve dialog asks to compare it with that box.

## v0.8.2

Released 2026-11-15.

### Added

- **One-line install** for every platform: `curl -fsSL https://github.com/patrick-1984/ai-minion/releases/latest/download/install.sh | sh` (Linux, macOS; `| sudo sh` installs into `/usr/local/bin`) and `[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; irm https://github.com/patrick-1984/ai-minion/releases/latest/download/install.ps1 | iex` (Windows preview). The scripts check the archive against `checksums.txt` and are signed release assets like `bootstrap.sh`.
- **Troubleshooting**: every command takes `-v`/`--verbose` (or `MINION_VERBOSE=1`) for debug logs (an explicit `--log-level`/`MINION_LOG_LEVEL` wins), with debug lines for the Worker's Hub connection, the Hub's request scheme decision, refused API requests, op routing and call streams; `minion worker status --json` adds `worker_id` and `approval`; new admin operation `hub_diagnostics` and page **Administration > Diagnostics** (version, uptime, public URL, trusted proxies, connected Workers, and how the Hub sees the current request). See [troubleshooting](docs/guides/troubleshooting.md).

### Changed

- A Worker that runs with a Hub address but no pairing and no invitation key (for example `minion worker up --foreground --share-existing all --server <url>`, also on Windows) now shows a pairing code by itself and connects once the code is entered in the Hub; before it only idled as "not enrolled".
- Hub identity: a Hub certificate is valid for 10 years (3650 days) by default and at most 3650 days (was 180, at most 400), so a normal deployment never has to re-issue it. The Workers page shows the certificate badge only from 30 days before expiry, and then says what to do.
- Hub identity page: behind an HTTPS reverse proxy that the Hub does not trust, a passphrase operation answered `tls_required` offers the button "I am behind a trusted HTTPS reverse proxy — continue" (fresh sign-in, CSRF, same-origin, JSON and lockout rules stay). The correct fix remains `MINION_TRUSTED_PROXIES` plus `X-Forwarded-Proto`.

### Fixed

- Hub identity page: the State card is a readable label/value grid again.
- Administration > Session control: sessions of all Workers are listed concurrently within the 2 s limit, so a stalled Worker no longer hides the sessions of the others ([#13](https://github.com/patrick-1984/ai-minion/issues/13)).

### Upgrade notes

- **Upgrade the Workers before the Hub's next certificate re-issue**: Workers older than 0.8.2 refuse a certificate longer than 400 days. A new 0.8.2 Hub issues a 3650-day certificate when its passphrase is first set, so pair only 0.8.2 Workers with it. After all Workers run 0.8.2, re-issue once (Administration > Hub identity) to get the 10-year certificate; nothing else needs attention after that.
- Protocol stays 3.0.

### Known limitations

- Follow-ups: [#22](https://github.com/patrick-1984/ai-minion/issues/22) (install.ps1 edge cases), [#23](https://github.com/patrick-1984/ai-minion/issues/23) (auto pairing with a concurrent `worker pair`), [#26](https://github.com/patrick-1984/ai-minion/issues/26) (debug log details).

## v0.8.1

Released 2026-11-15.

### Changed

- Compose: `deploy/compose/hub.yaml` takes the first administrator's password from `MINION_ADMIN_PASSWORD` in `.env` instead of the file `admin_password.txt` (used only on the first start; `MINION_ADMIN_PASSWORD_FILE` still works). Write it single-quoted into a `.env` readable only by you; see [Hub in Docker](docs/install/hub-docker.md).

## v0.8.0

Released 2026-11-15.

This release adds **session control**: an administrator can let chosen controller sessions (for example an AI agent running in a tmux session on a Worker) list, read and type into sessions on other Workers, from the Hub's web UI. See [session control](docs/guides/session-control.md).

### Added

- New admin page **Administration > Session control** and operations `session_control_list` / `session_control_set`. A grant names a controller session (Worker + session name) and its targets: **All**, or chosen Workers and/or sessions. No session has a grant until an administrator creates one; removing or reassigning a Worker deletes the grants of its sessions (Hub migration 0024).
- From a controller session, `minion`, `mn` and `minion mcp` (no `MINION_HUB_URL` needed) reach the granted targets through the local Worker and the Hub: `workers_list`, `sessions_list --hub-worker <id>`, `session_snapshot`, `session_capture`, `send_text`, `send_keys`, `set_task` and `answer`. Writes through MCP still need `--allow-writes`. `minion mcp` now also exposes `workers_list`.
- The effective permission is the grant (read, operate, message) ∩ the controller Worker owner's current permissions, checked on every call. A granted session's requests are accepted by target Workers without `pipe_accept_from` configuration; preview-only and the share selection still apply. Sessions without a grant behave as before, including call keys.

### Security

- A grant is keyed by the session name, which the controller's host reports and the Hub does not verify: any process of the Worker's user can use it, so a grant effectively applies to the controller host, and "All" reaches every Worker its owner may reach. See [security](docs/security.md).

### Upgrade notes

- Protocol stays 3.0; 0.7 and 0.8 connect to each other. Session control needs the Hub, the controller Worker and the target Workers on 0.8.0. A 0.7 target keeps its `pipe_accept_from` rule (the default `none` refuses a controller's writes; reads still work), and a 0.7 Worker daemon refuses the new forwarded requests of a 0.8 client, so nothing is sent to a same-named local session.

### Known limitations

- `workers_list` lists only Workers the grant reaches as a whole; take the id of a Worker reached through single sessions from its page in the web UI.
- The Session control page skips sessions of Workers that do not answer within 2 seconds ([#13](https://github.com/patrick-1984/ai-minion/issues/13)).

## v0.7.1

Released 2026-11-13.

### Fixed

- `minion hub identity` on a data directory without a Hub identity now says that the identity is created when the owner sets the Hub passphrase (`minion hub identity set-passphrase` or the banner in the web UI), instead of the 0.6 wording "when the Hub first starts".

## v0.7.0

Released 2026-11-12.

This release replaces the single plain Hub identity key with a two-tier identity: a master (root) key sealed with a passphrase that only the owner knows, and a short-lived operational key certified by it. It changes the network protocol to 3.0 and needs an owner action after the upgrade: see Upgrade notes.

### Security

- The Hub's root key is stored only sealed (`root.key.age`, age with scrypt) under an owner passphrase of at least 14 characters. The passphrase is never stored or logged. The plain 0.6 `identity.key` is sealed and removed when the owner sets the passphrase.
- The Hub signs its connections with an operational key that carries a 153-byte certificate from the root key (default validity 180 days, warnings from 30 days before expiry). Every certificate has a new operational key. Workers enforce the certificate's expiry themselves and refuse a certificate whose serial is lower than one they have seen.
- New admin page "Administration > Hub identity" and commands `minion hub identity status|set-passphrase|reissue|rotate-master|change-passphrase`. The two channels have different rules. On the web every passphrase operation needs an administrator who signed in within the last five minutes (fresh re-authentication) and HTTPS (the Hub's TLS, a trusted reverse proxy that reports HTTPS, or the management listener). The commands use the local admin socket `admin.sock` in the data directory instead: only the Unix user that owns the data directory (the user that runs the Hub) and root can use it, and there is no web re-authentication, CSRF or HTTPS rule because a browser cannot reach it. Both channels ask for the passphrase, are audited, and limit wrong passphrases per hour and lock after five failures, separately for the web and the terminal. The web refuses a passphrase equal to the signed-in administrator's local login password (only when the account has a local password); the commands do not check that.
- A Worker revoke is signed by the operational key of the session; a Worker that retires itself keeps the certificate ratchet, so an old certificate cannot make it forget its enrollment.
- Pairing can be resumed after a lost answer with the same code, and the owner can re-issue pairing for a Worker that never connected (10-minute window).
- Backups hold the sealed root (never the plain one, from the moment the passphrase is set) and the operational key; restore is two-phase and validates the identity per state.

### Upgrade notes

- Protocol 3.0 only (release manifest range `3.0..3.0`). Hub and Workers must all be upgraded; a 0.6 Worker is refused with `proto_incompatible` and retries.
- Order: take an encrypted backup of the 0.6 Hub, upgrade the Hub, set the passphrase (web banner or `minion hub identity set-passphrase`) — the Hub serves no Workers until then — then upgrade the Workers. A new Hub (no 0.6 identity) also serves no Workers until the passphrase is set. `minion update` on a 0.6 Worker only checks protocol compatibility when you pass `--hub-proto-range`; keep the order yourself. Existing pairings and `mnk_` invitations keep working.
- Recommended: rotate the master key once after all Workers have connected, because 0.6 backups hold the old root in plain form. A rotation revokes all unused invitations.
- There is no recovery for a forgotten passphrase; keep it outside the Hub.
- A Hub that sets `MINION_INFRA_INVITE_KEY_FILE` before it has a root key (a new Hub, before the passphrase is set) refuses to start; set the variable after the passphrase.

### Added

- `MINION_MANAGEMENT_LISTEN` (setting `management_listen`): an optional loopback plain-HTTP listener, the only place on the web side where the Hub accepts a passphrase without HTTPS. The local admin socket used by `minion hub identity` is a second place that needs no HTTPS, because it is not a network listener.
- `minion hub worker repair-pairing <worker>` and the REST operation `worker_pairing_reissue` re-issue pairing for a Worker that has never connected, with a button on its Worker page.
- New error codes `hub_cert_expired` (503), `reauth_required`, `reauth_unavailable`, `tls_required` and `passphrase_invalid`.

## v0.6.3

Released 2026-11-12.

### Fixed

- `minion worker status` opens the Worker database read-only and never runs migrations, so running it while a Worker starts can no longer make the Worker fail with `database is locked`; the Worker also retries a contended database setup within its normal busy timeout.
- Timing-sensitive tests (pairing, turn channel, keepalive, session retirement, enrollment) now synchronise on the event they wait for instead of a fixed timeout, so they no longer fail on slow CI machines.

### Added

- `docs/architecture/proposals/hub-identity-v2.md`: the draft design for the two-tier Hub identity planned for 0.7.0 (tracking issue #3).

## v0.6.2

The first published release of 0.6. The `v0.6.0` and `v0.6.1` tags exist but were never published as releases (their install smoke tests failed); everything below applies to 0.6.2.

Released 2026-11-11.

This release tightens how Workers, API keys, accounts and backups are authorized and protected. It changes the network protocol: see Upgrade notes before updating.

### Security

- Workers can no longer send to other Workers of the same owner without an explicit grant. A Worker's own identity now reaches only its own sessions. To allow cross-Worker messaging, give the sender a Worker-bound API key (`call_api_key_file`) and let the receiver accept it (`pipe_accept_from`).
- Every message-class request that reaches a Worker now carries an origin that the Hub derives (user, API key, or Worker). A Worker refuses a missing or malformed origin and records the origin in its local audit log.
- An API key with the `manage` permission can now be granted only on "everything". Narrower keys can no longer reset passwords, change roles or deactivate accounts. Existing narrow keys with `manage` keep working for everything else but are refused these account operations.
- Newly enrolled Workers (by invitation, pairing code or infrastructure key) start as pending. A pending Worker can connect but receives no operations until its owner or an administrator approves it. Existing Workers are approved by the migration.
- Invitations are bounded: non-administrators cannot create unlimited invitations (maximum one week), and reusable invitations need a use limit (default 10, maximum 100). Existing non-administrator invitations are capped by the migration. Infrastructure key files now have an expiry (`infra_invite_key_days`, default 30 days from first load; changing the setting does not change an existing key, so rotate the file secret or revoke the key), a use counter and sticky revocation.
- Access follows the person. Losing an identity-provider role no longer leaves orphaned access: API keys, sessions and Workers of an account without a grant are inert until a valid sign-in restores a role. Password reset and password change now revoke the account's API keys. A key being created while credentials change is refused.
- Administrator roles from an identity provider are granted only when a refresh token is available, and are re-checked at least hourly. Administrators without a usable token are suspended at start-up and audited.
- Hub backups are encrypted by default (age, passphrase). Restore validates the archive completely in a private directory before it creates or changes the destination, checks that the identity key matches the database, and enforces size and work-factor limits.
- Loading the Hub or Worker identity key now checks owner, mode and symlinks (reparse points and ACLs on Windows).
- Updated to Go 1.27.4, `golang.org/x/net` v0.62.0 and `golang.org/x/crypto` (fixed pseudo-version). `govulncheck` reports no known vulnerabilities.

### Changed

- Network protocol is now 2.0. Version 1.x peers are refused in both directions; there is no fallback.
- API keys expire after 90 days by default. Administrators can create a key without expiry with the `never` option (the "Never" choice in the web form, `never` in the API); it cannot be combined with an explicit expiry.
- `minion hub backup` requires `--passphrase-file FILE` or `--unencrypted`. `minion hub restore` takes `--passphrase-file` for encrypted archives and `--max-size BYTES` to change the size limit (default 64 GiB). Plain archives from earlier versions still restore.
- `minion hub user reset-password` and the web form accept `--keep-api-keys` to keep keys on a reset. A password change always revokes keys.
- The Worker's welcome message now carries a stable owner id next to the display name.
- The Worker local audit log records the origin of each request (migration, see below).
- Release workflow and manifest sample use protocol range `2.0..2.0`.

### Added

- Worker settings `call_api_key_file` (environment `MINION_CALL_API_KEY_PATH`, flag `--call-api-key-path`) and `pipe_accept_from` (`none` by default, `owner-workers`, or a comma list of Worker ids or fingerprints). Both are read at Worker start and cannot be changed by any Hub operation.
- `minion hub backup --passphrase-file` / `--unencrypted`; `minion hub restore --passphrase-file` / `--max-size`.
- Worker approval in the web UI (with fingerprint, host name, source address and a pending banner), through the REST API (`worker_approve`) and `minion hub worker approve <fingerprint|id>`. Approving by Worker id needs only the manage action on that Worker; approving by fingerprint also needs read, because the Worker is looked up first.
- `.github/dependabot.yml` for Go modules and GitHub Actions.

### Fixed

- A Worker started without a name on a host with a long host name no longer refuses to start: the default name is the short host name, cut to 64 characters.
- `bootstrap.sh` run as root no longer refuses its own cached binary: the binary is written by the script itself instead of keeping the owner recorded in the archive.
- Windows CI: test fixtures now use private file permissions, and the service fixture passes `--share-existing`. Production permission checks are unchanged.
- A running keyless call stream now ends when its owner loses access, instead of continuing until it finishes.
- Starting a user act-as session for an account without any grant is refused, and an active one ends when the target loses its grants.
- A failed or cancelled restore leaves an absent destination (and the parent directories it created) absent.

### Upgrade notes

1. Protocol 2.0: upgrade the Hub and all Workers together. A 1.x peer cannot connect to a 2.x peer, so mixed versions are not supported. Workers keep their enrollment and pin; no re-enrollment is needed.
2. New Workers need approval. After a new Worker enrolls, approve it in the web UI, with `minion hub worker approve <fingerprint|id>`, or with `minion hub worker approve <id>` using an administrator API key. Scripts and container setups that enroll and immediately operate a Worker need this extra step (see the Docker and pairing guides). Existing Workers are approved automatically.
3. Backups: scripts that run `minion hub backup` must add `--passphrase-file FILE` (a private file, mode 0600, owned by the running user, at most 4 KiB) or `--unencrypted`. Keep the passphrase separately from the backup; it cannot be recovered. A backup contains the Hub identity key, so disclosure of a decrypted backup is a Hub identity compromise and requires replacing the Hub identity and enrolling the Workers again.
4. API keys: keys created without an expiry now expire after 90 days. Existing keys are unchanged. Review long-running automation and set a longer expiry or ask an administrator for a key that never expires.
5. Password reset and change revoke API keys. Use `--keep-api-keys` on a reset if the keys must survive. Automation that relies on a user's key must be reissued after a password change.
6. Identity-provider administrators need a refresh token. Configure the provider to issue one (for example the `offline_access` scope) and have each administrator sign in again. Without it the account loses administrator rights (the audit log shows `role.suspend`) until it has one. Local administrators are unaffected.
7. Narrow keys with `manage`: such keys can no longer be created, and stored ones are refused password reset, role change and deactivation. Create an "everything" key if you need these operations.
8. Cross-Worker messaging: on the receiving Worker set `pipe_accept_from` (for example `owner-workers`), and on the sending Worker set `call_api_key_file` to a private file holding a Worker-bound API key; restart both Workers. Without these settings Workers can message only themselves.
9. Migrations run automatically at start and are forward-only. The Hub migrations (0019 and 0020) give a local user grant to accounts that have a password, suspend identity-provider administrators without a usable refresh token and record this in the audit log (0019), and add the approval state of Workers, approving every existing Worker and capping existing non-administrator invitations (0020). The Worker migration (0008) adds an origin column to the local audit log. Take an encrypted backup before upgrading; there is no downgrade.
10. Build tooling: Go 1.27.4 is required to build from source.

## v0.5.0 (not released)

Minion brings terminal sessions together through a Hub and its Workers, or runs locally in Solo mode.

- Open a session from its row to see the live screen and controls. Enter sends a message; Shift+Enter adds a line break. The focused Keys area sends navigation and editing keys to the session.
- Follow messages and reliably captured replies in Conversation. Delivery uncertainty is preserved, and writes are never replayed automatically.
- Configure Worker sharing, invitations, scoped API keys and optional OpenID Connect sign-in. MCP is read-only by default.
- Install the Hub or Worker using the guides in the README. The release provides verified package and image paths.

Windows Worker support is a preview. Reply capture requires a supported agent and tmux 3.8 or later on a suitable terminal screen. The CPace implementation has not received a formal external cryptographic audit. See the security guide for trust and deployment boundaries.
