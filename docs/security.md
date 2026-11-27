# Security and trust model

Back to the [README](../README.md).

Minion lets people, scripts and models read and type into terminal sessions on other machines. That is the product, and it defines what you are trusting. This page states the boundaries of the release it ships with, including what has not been reviewed yet. Where a rule came with a later release, the text names that release.

## Who sees what

- **The Hub sees the content of the sessions a Worker shares.** Screens, scrollback you capture, text you type, the messages you send an agent and the replies recorded from it pass through the Hub and, for the pipe, are stored in its database. A Hub operator, and anyone who can read the Hub's disk or a backup, can read them. Run the Hub on a machine you control, and share only the sessions you are willing to show it ([pairing and sharing](guides/pairing.md#choosing-what-the-hub-sees)).
- **A proxy that ends TLS for the web UI is trusted with all of it.** It sees every page and every event the browser sees. Run it yourself. To keep a proxy out of the content, let it pass the encrypted stream through and give the Hub a certificate of its own ([reverse proxy](guides/reverse-proxy.md)).
- **A session that is not shared is listed by name only**, to its owner and the administrators, so that they can choose to share it. The Hub never shows its content, state, task or history. The Worker enforces this itself, whatever the Hub or a caller asks for. When you stop sharing a session, the Hub stops serving what it stored of it; the stored history is hidden, not erased, and shows again if you share the session again.
- **The Worker's own user decides what is reachable.** A Worker runs as one Unix user (or Windows account) and can do to that user's tmux sessions everything the user can. A Worker in a container that mounts the host user's tmux socket has the same reach ([Docker Worker](install/worker-docker.md#the-trust-boundary)).
- **Roles:** an administrator may do everything, including manage users, Workers and settings; the owner of a Worker may read, operate and message on it; nobody else has any access. The one exception is a session on another Worker that was given access as described under [Access between sessions](#access-between-sessions). An API key never has more than its owner has, and is further limited to the resources and actions it was created with.

## Enrollment is not authorization: new Workers wait for approval

An invitation key, a pairing code or the infrastructure key file lets a machine **enroll**; it does not let that machine be **operated**. Every new Worker starts *pending*: it may connect, but the Hub routes no operation to it (no user, key or other Worker can name it as a target), its calls to the Hub are refused with `worker_pending` (it may still retire itself), and its sessions are not listed or read. The owner of the Worker or an administrator approves it in the Hub after comparing its fingerprint, host name and source address, with `minion hub worker approve`, or through `worker_approve` ([Approve a new Worker](guides/pairing.md#approve-a-new-worker)). So a leaked invitation key alone gives an attacker a pending, inert Worker that you can see and remove, not a way to read or type into anything, and not a Worker that can call other Workers.

What bounds a leaked key further: a user's key lasts at most one week, a user's reusable key allows at most 100 enrollments (10 by default), and the infrastructure key file expires (30 days by default), counts its uses and can be revoked from the Invitations page; reloading the file never resets any of this. Approve only a Worker you recognise: approval is the authorization, and an approved Worker has all the reach a Worker of its owner has.

## Worker isolation: one compromised Worker does not reach the others

A Worker identity can read and message **only its own Worker and that Worker's sessions**. It cannot reach another Worker, not even one of the same owner, unless the owner gives it a **call key**: an API key bound to that Worker with a narrow scope, set as `call_api_key_file` on the Worker ([API keys and MCP](guides/api-keys-and-mcp.md#let-a-worker-call-other-workers)). The receiving Worker then decides for itself with its local setting `pipe_accept_from` (`none` by default, `owner-workers`, or a list of Worker ids or fingerprints) whether it takes messages from another Worker ([the pipe](guides/pipe.md#agent-to-agent-the-call-key-and-pipe_accept_from)). The Hub tells every Worker who made each request (`origin`: user, API key or Worker, with the key and the sending Worker), and a Worker refuses a message-class request that has no valid origin. The setting can be changed only on the Worker itself, and each refusal is in the Worker's local audit.

What this protects, and what it does not:

- It protects every Worker against a **compromised peer Worker, or an agent on it that was talked into misbehaving**, as long as the **Hub is honest**. Enrolling a Worker, or stealing its identity key, no longer gives a way to type into the rest of the fleet.
- It does **not** protect against a compromised Hub or a malicious administrator. The Hub supplies the origin, and a Hub or an administrator can already create sessions and act as users. Trust the Hub as described above.
- Anything that runs as the Worker's own user can read that Worker's call key file and its local configuration (it can also change `pipe_accept_from` there). Run agents of different trust levels as different users or in different containers.

A session reaches another Worker without a call key only through the access that a person gave it in the Hub. That is the next section.

## Access between sessions

By default no session reaches a session on another Worker. Sessions of one Worker always reach each other through that Worker's local socket; no setting in the Hub separates them, and the Hub refuses a grant whose source and target are the same Worker, because it would promise a boundary that does not exist.

There are two ways to give a session access to another Worker, and both are decided by people in the Hub, never by the session itself:

- **A master session** ([Minion control](guides/minion-control.md), administrators only, 0.9.0). An administrator nominates a session of Worker A as a master session, and Worker B accepts control from Worker A, or from all Workers, optionally limited to some of its sessions. Both default to none. A master session has full control of what it reaches. A Worker that is not user-managed refuses its sessions to people and API keys (REST, the live lists, MCP); master sessions are not affected.
- **A grant** (**Access > Grants**). A grant says: this one named session of Worker A may reach that one session of Worker B, or every shared session of Worker B (future ones included), at one level. The source is always one named session, never a wildcard. With no grant in the Hub, nothing changes for anyone.

The stored grants of release 0.8 ("session control") can no longer be created: `session_control_set` with targets is refused, and a grant migrated from 0.8 only narrows a master session to its old targets ([session control](guides/session-control.md)).

### Levels

A grant has one of three levels. Each level includes the one before it. There are no deny rows.

| Level | What the source session may do on the target |
| --- | --- |
| View | List the Workers and sessions it reaches, and read those sessions. Its session list of the target Worker holds only the sessions it may read. |
| Message | View, and send a prompt to an agent. The Hub turns every `send_text` of such a caller into a submitted prompt for an agent pane only and refuses `force`; a target Worker that does not announce the feature `send_text.agent_only` gets no such request at all. |
| Control | Message, and type into any pane (`send_text`, `send_keys`), set the task and answer a dialog. |

No level allows creating sessions, changing what a Worker shares, or anything an administrator does. `force` never overrides a missing permission.

### Who decides, and what stands behind a grant

| Case | Who may create or widen it | Authority | It counts while |
| --- | --- | --- | --- |
| Both Workers have the same owner | that owner, or an administrator | `owner` | both Workers still have that owner |
| The Workers have different owners | an administrator only | `admin` | the administrator who decided is still an active administrator |

- Creating a grant, changing its level, expiry or note, and approving it need a **browser session** of a person. No API key, no Worker and no session can do it, so an agent cannot give itself or another agent access. An administrator who acts as another user is not an administrator for this purpose.
- **Revoking** only narrows. The owner of either Worker and every administrator may revoke, also with an API key that has `operate` on one of the two Workers.
- **Suspension.** When the administrator behind an `admin` grant loses the role or is deactivated, the grant is suspended in the same database transaction. It does not come back when the role comes back: an administrator approves it again and is from then on the person behind it. A suspended grant gives nothing and cannot be changed until it is approved.
- **End of life.** A grant is deleted when a session it names ends, is renamed or stops being shared, when one of its Workers is removed, and when one of its Workers changes owner. An expired grant gives nothing from the moment it expires; it stays listed as expired and is deleted 30 days later.

### What Claimed and Verified prove

- **Claimed** is every grant today, and every master session nominated by name. The source is a session **name that its Worker reports**; nothing verifies it further. Any program that runs as that Worker's user on that host can say that it is that session. Treat a grant to a Claimed source, and a name-only master session, as access for the **whole host and account** of the source Worker, and give it only to a host you trust as much as the target.
- **Verified** is a session-bound master (0.18.0, [prepare one terminal](guides/agents.md#prepare-one-claude-code-terminal)). The Worker pins one terminal process (Linux, tmux, a kernel with peer pidfd support, 6.5 or later) and the Hub issues a lease of 30 seconds for that one incarnation; the calls carry the lease, not a name. It is limited to listing Workers and sessions, reading a snapshot and sending a prompt, and, from v0.24.0, to the file ops `file_list`, `file_pull` and `file_push` when a grant or master acceptance carries the file flags ([file transfer](guides/file-transfer.md#from-a-session-on-another-worker)). Grants do not have a Verified source yet; the column for it exists and is not written.

Verified proves which terminal process calls. It does not prove what the agent in that terminal was told to do.

### What the Hub decides and what the target Worker enforces

- **The Hub decides every call.** It reads the grants of the calling Worker and session from its database on each call and keeps no positive cache. A grant counts only if it covers the target, is not suspended, has not expired, the source Worker still has the owner recorded in the grant, and, for an `admin` grant, the person behind it is an active administrator now. A running call is checked again within 30 seconds and once more right before a request is passed on; a grant that was revoked, changed, suspended or expired ends it. When the Hub cannot read the grants, the call is refused.
- **The target Worker enforces its own rules whatever the Hub says:** a session that is not shared is refused, and a preview-only Worker refuses every write. The target does not consult `pipe_accept_from` for a request the Hub marks as granted (`origin.grant`).
- **The target Worker does not learn which session called.** The origin it receives names the source Worker and its owner and says that a grant applies; the source session and the grant are known to the Hub only, and are in the Hub's audit.

**Audit.** Every creation, change, approval and revocation of a grant is an audit event with the grant, its source session, level and authority. A write that a session makes on another Worker names the calling session, and a refused one is recorded as `denied`. Audit details are a fixed list of `key=value` facts; they never contain screen content or typed text.

**Seeing it.** **Access** shows who reaches what as a table of source sessions and targets, and **Why?** explains one pair check by check. Both are read-only and show access, never content. A person who owns only one end of a pair sees the other end without its session name.

### What is not protected

- A **Claimed** source is not an identity (see above). Run agents of different trust levels as different users or in different containers, on different Workers.
- A **compromised Hub or a malicious administrator** is not stopped by any of this: the Hub makes the decision and supplies the origin.
- **Message is not harmless.** A prompt to an agent can make that agent run commands with its rights ([next section](#a-message-can-make-an-agent-run-commands)).
- A grant on **all shared sessions** of a Worker follows that Worker's sharing: a session shared later is reachable at once.
- Revocation is not instant for a call that is already running: it ends within 30 seconds.

## A message can make an agent run commands

Sending a message to an agent (`pipe_send`, `send_text`, `send_keys`, `answer`, `set_task`) is the same as typing into it. The agent, or the shell it sits in, may act on that text. Anything that can send, and anything a sender can talk into sending, can therefore cause commands to run on that machine, through that agent, with that agent's rights. The same is true of a model that reads a session and then writes to another: what it reads can steer what it types.

Consequences you should draw:

- Give a person, key or model the right to send (`operate`, `message`) only on the Workers and sessions where you accept that.
- The MCP server is **read-only by default**; the tools that type, create sessions, send messages or change sharing appear only with `--allow-writes` ([API keys and MCP](guides/api-keys-and-mcp.md)).
- Replies recorded from an agent are the agent's output, not a person's words. A model receives received turns only from sources its user has marked as **trusted** ([the pipe](guides/pipe.md)).
- Writes that are guarded (for example, a pane that changed since you looked, a shell prompt where an agent was expected) are refused with a reason and are repeated only when you pass `--force` yourself; nothing forces a write on your behalf. With tmux the check and the paste are one step; with psmux on Windows they are two requests, so a program that exits exactly between them can leave the text to the shell that follows it ([Windows Worker](install/worker-windows-preview.md)).

## Credentials

- **Invitation keys** (`mnk_...`) enroll Workers. **API keys** (`mn_...`) call the Hub. Both are shown once; keep them in files that only their user can read. A key given **literally on a command line is visible in the process list** of that machine to every local user; use a file (`--invite-key <file>`, `MINION_INVITE_KEY_FILE`, `MINION_API_KEY_FILE`). The API key file is checked: a regular file, owned by you, closed to group and others, not a symbolic link. A key left on disk is a credential until it expires or you revoke it. Even a stolen key only enrolls a pending Worker (see above).
- Secrets are never accepted as command-line values and are redacted in logs. The one exception is the invitation key, which is meant for one-command setup and carries the warning above. A `curl` call to the Hub's HTTP API must not carry the key in its arguments either: give it the header on standard input ([API keys and MCP](guides/api-keys-and-mcp.md)).
- **Keys follow the person.** An API key or Worker identity has only the access its owner has now: when the owner has no effective role (the IdP removed every IdP role and no local grant remains) it is inert at once and returns with the role. Resetting or changing the owner's password revokes all the owner's API keys in the same transaction (an administrator can keep them on a reset). A key created without an expiry lives 90 days; only administrators can create keys that never expire. IdP admin roles are granted only with a refresh token and re-checked at least hourly; existing tokenless IdP admins were suspended on upgrade (sign in again).
- **Backups and release caches are credentials too.** A Hub backup contains the data key, the TLS key and the Hub's identity files, and which identity files depends on the state of the Hub: none before the identity exists, the **plain** identity key when the passphrase is not set yet (state B1, which includes every Hub just upgraded from 0.6.x), the sealed root (`root.key.age`) once the passphrase is set, and the operational key (`op.key`) once a certificate exists ([backup and restore](guides/backup-restore.md)). `minion hub backup` encrypts the archive with a passphrase by default (`--unencrypted` is an explicit opt-out); that protects the file at rest only and is a second layer on top of the sealed root, not a replacement. Keep the archive passphrase apart from the archive, and the Hub passphrase apart from both.
  - **If a backup is disclosed**, read it by what it held. The **operational key** lets the thief impersonate the Hub to a Worker that has not yet seen a later certificate serial, until the certificate that names it expires: re-issue the certificate at once (`minion hub identity reissue`, or the Hub identity page), and Workers that see the new serial reject the thief's key. The **sealed root** is safe as long as the Hub passphrase is strong and unknown to the thief; if the passphrase is weak or may be known, treat it as a root compromise (below). **Changing the passphrase does not help** against a thief who already holds the file, because the old file still opens with the old passphrase; a master-key rotation helps only for the future. A backup made by 0.6.x, or in state B1, holds the **plain** identity key: its disclosure is a root compromise (see the rotation advice under [migration](#upgrading-from-06)).
  - **A root compromise** (the root key and its passphrase are both in an attacker's hands, or a plain identity key was disclosed and not rotated away before the thief used it) is not repaired by rotation, because a thief with the old root can sign a competing transition. The Hub identity is replaced by a new one and every Worker is enrolled again (reset on its machine, enrolled with a new invitation key; see [backup and restore](guides/backup-restore.md#if-a-backup-is-disclosed)).
- **Two secrets are printed on purpose, and nothing else is.** Protect the logs that hold them (the Worker's console and log, and the Hub's first-start log), and do not paste them into tickets or chat.
  - A **pairing code** (`MN-XXXX-XXXX`) is valid for 10 minutes and is printed on the Worker's console and in its log.
  - A **generated administrator password**: when the Hub starts for the first time without `MINION_ADMIN_PASSWORD_FILE` (or `MINION_ADMIN_PASSWORD`), it makes a random password for the first administrator and logs it once, at error level, so that no log threshold hides it. It must be changed at the first sign-in, but until then the log line is a working credential. To avoid it, give the Hub a private password file (a regular file only the Hub's user can read, mode 0400) as in the [binary](install/hub-binary.md#run-it) and [Docker](install/hub-docker.md#prepare-and-start) guides; a configured password is never logged. If the password was generated, change it, then remove or rotate the log that shows it.

## Browser sessions

A browser session is a random cookie (HttpOnly, Secure, SameSite=Lax); the Hub stores only its hash. It ends in these ways:

- **Idle lifetime** (`session_ttl`, `MINION_SESSION_TTL`, default **2 hours**, in **Administration > Hub settings**; 5m to 720h are accepted, and a value above **12 hours** acts as 12 hours). A request the person makes on the Hub's own pages (a page load or navigation, a click, a form, an htmx action the person triggered) moves the end to now plus the idle lifetime. It counts only with affirmative evidence from the browser: Fetch Metadata `Sec-Fetch-Site: same-origin`, or `none` together with `Sec-Fetch-User: ?1` (an address the person typed, a bookmark, a reload); from a browser without Fetch Metadata, an `Origin` (or else `Referer`) equal to the Hub's full public origin (scheme, host and port). Requests the page sends on its own (timed polls, refreshes after a live event, live streams and their periodic authorization re-checks, the internal API calls the Hub makes for them, and the routes pages poll, also from tabs that were opened before an upgrade) never extend it, so an open tab that nobody uses still expires. Requests another site caused and requests without such evidence (a program, a client that sends neither header) never extend it either.
- **Absolute lifetime.** A session started with a local password ends **12 hours** after the sign-in, however active. A session started with OIDC ends **7 days** after the sign-in. These caps are fixed and do not depend on `session_ttl`: no idle end, the first one included, is ever set past them or more than 12 hours ahead.
- **Lowering `session_ttl`** applies to sessions that are already open on their next request: their idle end is moved to now plus the new value when it was further away. Sessions issued by **v0.20.4** (a fixed 24 hours) are treated the same way on their first request after the upgrade, and their absolute end counts from their sign-in. Their cookie keeps the expiry v0.20.4 gave it (24 hours after that sign-in), so such a browser signs in once more at most a day after the upgrade.
- **Sign-out, a password change or reset, deactivation, or the loss of every role** end it at once, as before.

Nothing extends or renews a session whose idle lifetime ran out: the Hub never serves a request with it again.

**Signing in again silently (OIDC).** When the idle lifetime of a session that was started with OIDC has run out, the person's next **page load** on the Hub (a refresh, a link, the address bar; with the evidence above, and never an htmx action, a fetch, a poll, a stream or a page load another site caused) is sent to the identity provider's authorization endpoint with `prompt=none`. It is the ordinary sign-in request (state, nonce, PKCE, the browser binding cookie, the return path checked to be a path on this Hub), only asking the identity provider not to interact with the person. While the identity provider's own browser session is valid it answers at once: the Hub signs in through the ordinary sign-in path (a **new** session with a new cookie, the role rules applied to the fresh claims, a deactivated account or no role refused, audited as `login`) and returns to the same page. When the identity provider answers `login_required`, `interaction_required`, `consent_required` or any other error, the Hub's sign-in page is shown, and it returns to the same page after the sign-in. A session started with a local password, or a page load without the evidence above, goes to the sign-in page. No refresh token is used for this.

Every other request of an expired session gets a quiet `401` with the header `X-Minion-Session: idle` (details `reason: session_idle` in the API error, and `unauthenticated` in the `access_lost` event of a stream) instead of a redirect. Pages show "Session paused" with a **Continue** button; Continue (or a refresh) reloads the page, and that page load signs in again as above. Every other control keeps working as it is: **Sign out** signs out. The inspector and the terminal view pause on any missing sign-in (an expired or an absent cookie alike): they keep their kept frames, drafts and screen hidden, send nothing, and destroy nothing without the person; only a verified other principal (another account, an act-as that ended) clears them. On Continue (the terminal: on the next click) they first ask quietly who the request is now: when another tab has signed in again as the same principal (the same account, role and act-as setting), they resume without a reload (the inspector keeps its kept sessions hidden, never cleared, until the page's CSRF token is the current one, since a renewed cookie has a new token), and a terminal sends again only after a fresh read of its screen. A terminal that nobody uses does not reload itself on Worker events (a page load would count as the person's activity): it shows "Out of date" and reloads on the next click.

**Ending access.** With OIDC, ending the person's browser session at the identity provider (in authentik: end the user's sessions on the user's page, or deactivate the user there) prevents a **later** silent sign-in: once a Hub session's idle lifetime has run out, the silent sign-in fails and the sign-in page asks for a fresh sign-in. It does not end a Hub cookie that already exists: the Hub does not ask the identity provider on each request, so while that cookie keeps being used in the foreground its idle end keeps sliding, up to the absolute end of the session (7 days after an OIDC sign-in). To end access **at once**, for example for a stolen cookie, do it in the Hub: deactivate the user (**Administration > Users**, or `minion user_deactivate`), which deletes every session of the user immediately and refuses every later sign-in; for a local password account, resetting the password (`minion user_reset_password`) also ends all its sessions. The Hub has no separate "sign out all sessions" action for a user who should keep access. A person signs out their own browser with **Sign out**. A stolen Hub cookie is usable until its idle end (as long as the thief keeps using it, up to the absolute end of the session); it cannot be turned into a new session without the person's identity provider session.

## Pairing and the Hub identity

A Worker pins the Hub's **root key** (its fingerprint) when it pairs or enrolls; later it trusts that key, not any TLS certificate. Since 0.11.0 you can give the Worker the fingerprint you expect before the first contact, so that it pairs only with that Hub ([trust without certificates](#trust-without-certificates)). A proxy, a tunnel or a certificate change therefore does not weaken or break it, and a Hub that cannot prove it holds the pinned root is not trusted. A master-key rotation is a signed chain that Workers follow without pairing again. The operations that touch the root (set the passphrase, re-issue the certificate, rotate the master key, change the passphrase) need the administrator role, a sign-in from the last five minutes, the CSRF token, a same-origin request and an effective HTTPS connection (or the administrator's explicit confirmation on the page that a trusted HTTPS reverse proxy is in front; the correct fix is `MINION_TRUSTED_PROXIES` with `X-Forwarded-Proto`); API keys cannot perform them, and a passphrase is never stored or logged. See [Hub identity](guides/hub-identity.md).

**The pairing handshake (a password-authenticated key exchange over the pairing code) has not had an independent cryptographic review yet.** Until it has, pair on networks and with codes you would also trust for a password prompt, keep the code short-lived (it expires after 10 minutes), and prefer invitation keys created in the Hub for unattended enrollment.

### Two keys: a sealed root and a short-lived operational key

Since 0.7.0 the Hub does not sign its handshakes with the long-lived identity key. There are two keys ([design](architecture/proposals/hub-identity-v2.md)):

- The **root key** is the identity Workers pin. It lives on the Hub only in an encrypted file, `root.key.age`, sealed with a **passphrase** that you type and keep elsewhere. The Hub opens it only for the moment a certificate or a rotation is signed, then wipes it.
- The **operational key** (`op.key`) is plain on the Hub's disk, so the Hub can restart unattended. It signs the handshake. The root certifies it with a **Hub certificate** (153 bytes: both public keys, a serial, a start and an end time, and the root's signature). A certificate is valid for **10 years (3650 days)** by default (1 to 3650 when you choose) and **every certificate carries a new operational key**. Nothing renews a certificate by itself, because that needs the passphrase and the Hub never holds it; with the 10-year default a normal deployment never has to re-issue, and nothing needs attention while the certificate is valid. A Hub whose certificate was issued by an earlier version (180 days) gets the long certificate at its next re-issue: re-issue it once, from the Hub identity page or with `minion hub identity reissue`, after updating the Workers to the Hub's version (Workers of earlier versions refuse a certificate longer than 400 days) ([Hub identity](guides/hub-identity.md)). The pages warn only from 30 days before expiry. The trade-off: a stolen operational key can impersonate the Hub until its certificate ends, so after a suspected theft of the data directory re-issue the certificate at once.

What this gives, and what it does not:

- A copy of the disk, the data directory or a backup does not give the root unless the thief also learns the passphrase, and the passphrase can then only be guessed offline at the speed of the key derivation. The thief does get the operational key, which is limited by the certificate (above) and ended early by a re-issue.
- A stolen administrator session cannot issue or rotate anything: every identity operation needs a **fresh re-authentication** (5 minutes) and the passphrase, and is refused over plain HTTP.
- It does **not** protect against an attacker who is inside the running Hub while you type the passphrase (web page or command): the passphrase can be captured then. A proxy that ends TLS sees the passphrase of every web operation; if you do not trust it, use the command line, which talks to the Hub over a local socket (`minion hub identity ...`, as the Hub's operating-system user).
- It does not protect against a root compromise (key and passphrase both lost), and it does not make the Hub trustworthy for session content (see above).

### The passphrase

- At least **14 characters** (counted as Unicode characters, at most 1,024 bytes), entered twice when you set, change or rotate. Use five or more random words, or 20 or more random characters. The Hub checks only the length, that a change differs from the old one, and, on the web only and only when you have a local password the Hub can check, that it differs from your login password (an account that signs in through an identity provider has no such password, and the command line does not check it); a long weak phrase passes, and the strength is yours to judge. It is used byte for byte as typed: plain ASCII words avoid a keyboard that composes accented characters differently.
- The key derivation is age's scrypt with **work factor 18** (about 256 MiB of memory and about one second on a current server CPU). The Hub reads a sealed file only at that factor.
- The passphrase is **never stored** (no file, database, environment variable, keyring or cache), **never logged** (audit entries and error messages do not contain it; request bodies of the identity operations are kept out of logs) and is accepted only in the body of the operation request, never in a URL, header, argument or environment variable. The Hub wipes its copies after use as far as it can; this is best effort, not a guarantee.
- Wrong guesses are limited online: the web and the local command have separate counters that survive a restart, a lock after every 5 failures (15 minutes, doubling up to 4 hours) and 10 attempts per hour. This limits online guessing only; against a stolen copy of the file the length of the passphrase and the work factor are the protection.
- **A forgotten passphrase cannot be recovered**: no escrow, no reset, no second key. The Hub keeps serving until its certificate expires; after that the only way back is a new Hub identity (a new data directory) and re-enrolling all Workers. Keep the passphrase in a password manager or on paper, apart from the Hub and its backups.

### Worker clock and expiry

The Worker checks the certificate itself. It refuses a Hub whose certificate starts more than one hour after its own clock (`cert_not_yet_valid`: a Worker clock that is too far behind), and it ends a connection when the certificate's end time is reached, on both its wall clock and its monotonic clock, so a Hub that keeps a connection open past expiry is cut by the Worker. A Worker clock that runs ahead ends trust early by the same amount. Keep Worker clocks synchronized (NTP). Expiry never unpairs a Worker and never touches its pin: when the Hub certificate has expired the Hub refuses new Worker connections (`hub_cert_expired`) and closes the open ones, the web UI and the commands keep working, and Workers reconnect by themselves after you re-issue. Workers also reject a certificate with a lower serial than one they have already seen, or a different certificate with the same serial, so an older stolen certificate does not come back.

### Upgrading from 0.6

Protocol 3.0 needs the Hub and all Workers upgraded together. A 0.6 Hub already has its identity key in plain form; the upgraded Hub keeps that key as the root, so no Worker is paired again and every invitation key still works, but **the Hub does not serve Workers until the owner has set the passphrase** (web banner "Set the Hub passphrase", or `minion hub identity set-passphrase`). That seals the key and issues the first certificate. Copies of the old plain key remain in your pre-0.7 backups, so a **master-key rotation after the upgrade is recommended**: upgrade the Hub, set the passphrase, upgrade the Workers and let them connect, then rotate (`minion hub identity rotate-master`); Workers follow the chain at their next connect. A rotation revokes unused invitation keys, and it cannot undo a theft that happened before it. Details: [Hub identity](guides/hub-identity.md).

## Trust without certificates

A Worker does not trust a Hub because of its TLS certificate. The chain is: the **Hub fingerprint** names the root key; the root signs the certificate of the operational key; the operational key signs every handshake; the channel keys come from that handshake. A proxy, or anyone in between, can hold a valid certificate for the address, but not the root. Release 0.11.0 adds no cryptography to this chain: one comparison before the pairing code is used, and one optional, informational member in the welcome.

What the Worker compares the Hub with, shown as **Hub check** in `minion worker status`:

| Hub check | Where the expected fingerprint came from |
| --- | --- |
| `matched the fingerprint you gave` | the setting `hub_fingerprint`, the answer to the prompt, or `minion worker verify-hub` |
| `from the invitation key` | the fingerprint inside the invitation key |
| `not verified` | nowhere: the Worker paired on first use, with whichever Hub answered |
| `DOES NOT MATCH the fingerprint you gave - not connecting` | the Worker is paired with another Hub than the one you named; it stays disconnected |

### What is protected against a proxy that turns malicious

The case that matters is a proxy that ends TLS with a valid certificate (yours, or a company's TLS inspection) and then works against you.

| Situation | A malicious TLS-terminating proxy |
| --- | --- |
| The Worker's expected fingerprint was read on the **Hub host** (or on a Worker that was checked that way), or the invitation key was created there and reached the Worker **without passing through a page the proxy can rewrite** | **safe**: it cannot read, change, replay or impersonate the Worker channel. It can relay, delay or drop the connection, and that raises no alarm. |
| The fingerprint or the invitation key was copied from the **web UI** | **not protected**: a proxy that rewrites that page can plant its own Hub, and the Worker cannot tell. |
| First use without an expected fingerprint (`not verified`) | **not protected** when the proxy also sees the typed code. `minion worker verify-hub` with the value from the Hub host detects it later and stops the Worker. |
| A paired Worker, and the proxy appears later | **safe**, with an alarm: the Worker does not connect ("HUB IDENTITY CHANGED"). |
| The web UI in a browser | **not protected**. |
| The CLI and MCP with an API key | **not protected** without a TLS key pin. With `hub_tls_pin` read on the Hub host, a TLS endpoint with another key is refused ([API keys and MCP](guides/api-keys-and-mcp.md#use-a-key)). |
| The Hub side: which Workers exist and are approved | **not protected**: a proxy that holds an administrator's web session can enroll and approve Workers of its own, or misuse the genuine Hub. |

### Pairing requests

Since 0.12.0 an administrator can admit a waiting Worker from **Pairing requests** instead of typing its code ([pairing](guides/pairing.md#admit-a-waiting-worker-pairing-requests)). The pattern is NetBird's peer approval combined with the fingerprint comparison of SSH host keys and Signal safety numbers; no new cryptography: the Hub answers with `pair.admit` instead of CPace and signs the handshake transcript with its certified operational key as in every mode, and the Worker signs it with the key the request showed.

| Admitted Worker | What is protected |
| --- | --- |
| The administrator compared the Worker fingerprint **and** the Worker's console showed this Hub's words | **safe**: the Hub enrolls exactly the Worker key it showed (another key fails the Worker's signature), and the Worker pins exactly the Hub key it showed. |
| The Worker has `hub_fingerprint` | **safe** on the Worker side as with a code: another Hub is refused before anything. |
| Nobody compared the words on the Worker side, no `hub_fingerprint`, a trusted TLS certificate | **as safe as that certificate** (the WebPKI trust NetBird's peers place in their management server): whoever holds a valid certificate for the Hub's host name, including a TLS-terminating proxy or another server that shares a wildcard certificate's key, can admit the Worker to itself. The Worker's status shows the Hub check `not verified`; `minion worker verify-hub` detects it later. `hub_fingerprint` is the strong gate. |
| No `hub_fingerprint`, and the upgrade was redirected, went through an HTTP proxy, used plain `ws://`/`http://`, or the certificate is not trusted for the host name | not offered: the Worker pairs only with its code. Only the TLS connection to the configured host itself that carried the upgrade counts. |
| A later connection with the same code presents another Hub key than the box showed | refused before the code is used; the Worker shows a new code with the new words (and logs a warning). |
| The administrator did not compare the Worker words | **not protected**: another machine waiting at the same moment could be admitted. A request always names the Worker key and the Hub key of its handshake, and the admission is refused when either changed after the list was read, or when the Hub's master key was rotated since. |

A Worker that comes back to a pairing the Hub already committed (its answer was lost) is never listed: it completes with its owner's code. An admission never enrolls a key the Hub already knows. Only an administrator, or an administrator's key whose scope reaches the whole Hub, sees and admits requests.

More limits, stated plainly:

- **A correct Hub fingerprint does not prove which account claimed the Worker.** The paired box shows the **Owner** first; that line is the check ("Not your account? Run: minion worker reset").
- The pairing code has 20 secret bits: one online guess per code, then the code is retired. With an expected fingerprint a proxy that guesses or sees the code still cannot pair the Worker with another Hub.
- **The TLS path is information, never a verdict** ([reverse proxy](guides/reverse-proxy.md#what-a-worker-reports-behind-a-proxy)). "keys match" is a statement about a key: a proxy that holds the Hub's own TLS key shows the same. The Worker channel never depends on TLS.
- The words are the first 66 bits of the fingerprint: six words from the BIP39 English word list (not a BIP39 mnemonic). Finding a key with the same six words takes about 2^66 work against a fixed key, roughly 230 GPU-years with incremental key generation. Fewer words are much weaker, so the Worker always compares all six; copied and automated values use the full 52 characters.
- The handshake still awaits an independent cryptographic review ([above](#pairing-and-the-hub-identity)).

The recipes that keep the proxy out of these steps are in [pairing](guides/pairing.md#when-you-do-not-want-to-rely-on-the-proxy).

## Updates

`minion update` installs only a release whose signature identifies this project's release workflow for that exact tag, recorded in the Sigstore transparency log, and whose archive matches the signed checksums. The `bootstrap.sh` installer has two modes, and they are not the same:

- **Signed release (the default):** it verifies the same signature of the checksum list as `minion update` does (with a pinned `cosign`), then the archive against those checksums.
- **Checksum-pinned (an exact version plus `MINION_SHA256`):** it checks only that the archive has the SHA-256 you supplied. It does not fetch or verify the checksums file, the signature or the transparency-log entry, so the signature guarantees above do **not** apply to this mode. Take the checksum from a source you trust independently of the download (for example the signed `checksums.txt` that you verified yourself), because the installer trusts it completely.

 There is no database downgrade: an older Hub refuses a database a newer one changed. `minion update` has a check that refuses a release whose protocol range does not overlap the Hub's, but in this release the source of the connected Hub's range is not wired in production: **without the explicit flag `--hub-proto-range MIN..MAX` (for example `--hub-proto-range 3.0..3.0`) the updater performs no protocol check and installs the release**. An older Worker therefore does not protect itself; follow the upgrade order yourself (Hub first, see [Upgrading from 0.6](#upgrading-from-06)). Release 0.7 (0.7.0 and later) speaks wire protocol **3.0** only (the signed release manifest carries the range `3.0..3.0`): there is no fallback to 2.x, a 0.7 Hub and a Worker of 0.6 or earlier cannot connect (a 2.0 Worker is answered `proto_incompatible` and retries until it is upgraded), and mixed 0.6 and 0.7 installations are not supported, so upgrade the Hub and all Workers from 0.6 together. 0.7.x Hubs and Workers interoperate.

**Updates from the Hub (0.9.0).** An administrator can update Workers to the Hub's release (`worker_update`) and, with the opt-in Docker overlay, the Hub itself (`hub_update`). In both cases the binary or image comes only from a release signed by this project's release workflow (the Hub pulls the image by the digest in the signed release manifest); a downgrade is refused, the Worker checks the protocol range of the Hub that asked, and a Worker refuses with `remote_update off`. A compromised Hub can therefore move Workers only to another signed release, not run code of its own on them. The Docker socket that the Hub's self-update needs is root-equivalent on that host: mount it only where you trust the Hub as much as root. The daily release check contacts only `api.github.com` (`update_check off` contacts nothing).

## File transfer (0.24.0)

File transfer copies files between the folder of a session and a caller; the Hub relays the bytes and keeps no content ([file transfer](guides/file-transfer.md)).

- **Both ends decide.** The Hub decides the caller's file action (`file_read` for a pull, `file_write` for a push) and the Hub policy at admission, every 15 seconds while bytes move and at a commit gate before it forwards the sender's final trailers: without them the receiver cannot verify and publishes nothing. A revoked API key, grant or acceptance, or a policy switched off, stops a running transfer.
- **Only named authority.** An API key holds a file action only when it names it (a wildcard or "everything" scope does not); grants and acceptances need the `files_pull`/`files_push` flags, and only a Verified source carries them. A Worker identity has none.
- **The Worker enforces locally whatever the Hub says:** its switch `file_transfer` (the Hub cannot lift it), sharing, the folder of the pane as the only root (a pinned directory handle; no `..`, absolute path, symbolic link or other mount is followed), the system, home and sensitive folders, push-protected names (`.git/hooks`, shell rc files, agent and CI configuration), entry types, limits and the single-use leg token bound to its connection. A compromised Hub cannot read or write outside shared session folders, cannot push into `.git/hooks` and cannot turn a switched-off Worker back on.
- **Nothing runs.** Transfer code never executes content, never sets an exec bit, never writes outside the folder and creates no autostart entry.
- **Verification before publication:** per-file SHA-256 and size against the manifest, the manifest digest, the end of the archive and the trailers; then an atomic publish that never replaces an existing name unless asked (`--overwrite`, regular files only).
- **What it does not protect against:** an agent in the pane choosing its folder with `cd` before a transfer; a person or agent running a pushed file later; transfers that a caller with the authority asks for. The history (who, which sessions, which grant, counts, manifest digest) is visible to the target's owner; logs, metrics and audit details never contain names, paths, tokens or content.

## Installing Android apps (0.25.0)

The Minion app on an Android Worker installs other apps from APK files in its Minion files folder ([Installing Android apps](guides/android-apps.md)). Installing an app is powerful: an installed app runs with its own permissions and can start at boot.

- **Who may install.** An administrator on every Android Worker, an owner on their own; an API key only when its scope names the action `app_install` (no wildcard, `everything`, `operate` or file action implies it); a session on another Worker only from a **Verified** source (a bound master, or a grant bound to the session) whose Control grant or master acceptance has the flag `apps_install` (default off, never implied by `files_push`), on the master lane. A claimed source and a Worker identity have none. The Hub audits every request with the package, the version code, the signer's SHA-256 and the outcome.
- **The first install needs a confirmation on the device.** Android asks once, on the device, before an app that Minion did not install is installed or updated; the Hub cannot answer that dialog. The Worker and the app check the path (only inside Minion files, no symbolic link), the SHA-256 of the file from the request to the commit, the signer (the same as the installed app) and the version (no downgrade); a new app needs `allow_new`; the Minion app itself is refused.
- **Updates of apps Minion installed are silent** (Android 12 and newer): whoever may run `app_install` on the Worker can replace such an app with any newer version signed with the same key. The signer check keeps a different key out.
- **The switch on the device wins.** Turning **Install unknown apps** off for Minion stops every install (and self-update); the Hub cannot turn it back on.
- **Residual risks.** Every terminal session on the Android Worker runs as the Minion app's user and can write the app's private files, including the install request: treat a session on the device as able to request installs (a first install still needs the press on the device, an update of an app Minion installed does not), and do not run untrusted agents there. Play Protect may scan or block an install, and is not tested on every device. Minion never uninstalls an app.

## When something is not known

- Reads that fail are retried or reported as errors. A write that may have been delivered, but whose answer was lost, is reported as **unknown** and is never replayed for you; you repeat it with the same request id on purpose.
- A change of sharing whose answer was lost is treated as possibly made: the Hub hides what the change would have hidden until the Worker confirms its rule.
- While the Hub does not know a Worker's sharing rule it hides that Worker's stored history instead of showing it.
- Sign-in through an identity provider fails closed: a claim that is missing or matches no configured value gives no role, and no role means no access ([OIDC](guides/oidc.md)).

## The public presence files of a root Worker

A Worker that runs as root publishes a presence record in `/run/minion-presence` (`/var/run/minion-presence` on macOS), owned by root, mode 0755 for the directory and 0644 for the files, so that every program on the host can see that it runs and which version it is ([integrate with local programs](guides/integrate-local.md)). The record holds: the daemon version, the scope, the process id, timestamps, a random instance id and the scheme, host and port of the Hub. If the Hub nominated usage, a second public file holds the nominated labels and aggregate meters, including the Hub owner's personal pair; the Hub UI says so before it publishes. They never hold keys, tokens, pairing codes, key words, the Hub owner or Worker name, session data, folders, member ids or account labels.

- **Threat model:** every local account can read these files and can learn that the Worker exists, its version and the Hub address. No account can write there: the directories are root-owned, writers stage files in a root-only directory and rename them, and readers refuse a link, a non-regular file, a wrong owner or mode and an oversized file.
- A Worker of a non-root user publishes only in that user's private directory (0700, files 0600); another user cannot see it.
- The Hub controls only the content of the usage document. The Worker writes the presence fields and accepts a usage document only from its own Hub connection.
- Consumers must verify what they read; labels are untrusted text. A record of a crashed Worker goes stale after 90 seconds.
- **Stopping a Worker does not remove the usage file.** A normal stop marks the presence record as stopped and leaves `<instance>.usage.json` in place, readable by every local account. To end the publication, withdraw the nomination or the recipient's access while the Worker is online (the Hub then tells the Worker to remove the file), check that `<instance>.usage.json` is gone from the presence directory, and only then stop the Worker. If the Worker is already stopped, delete that instance's `.json` and `.usage.json` files as root.

## Not covered, or not finished

- The Windows Worker is a **preview**: its sessions end with the Worker process, and a Windows service runs as the LocalSystem account ([Windows preview](install/worker-windows-preview.md)).
- The pairing handshake is awaiting independent cryptographic review (above).
- The Hub passphrase has no recovery path (above), and a Worker that stays offline for the whole remaining life of a stolen certificate is not protected by a re-issue.
- A change of sharing made on the Worker's machine reaches the Hub within the Worker's poll interval (one second by default). Changes made through the Hub apply before the Hub answers.
- Local users of the Worker's machine are not an adversary the Worker defends against: the control socket and the state directory are private to the Worker's user, and anything running as that user is already inside the boundary.

Do not put the details of a suspected vulnerability in a public issue; use the repository's private security reporting (GitHub security advisories) when it is enabled.
