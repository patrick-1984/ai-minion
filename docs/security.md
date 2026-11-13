# Security and trust model

Back to the [README](../README.md).

Minion lets people, scripts and models read and type into terminal sessions on other machines. That is the product, and it defines what you are trusting. This page states the boundaries as they are in release 0.7.1, including what has not been reviewed yet.

## Who sees what

- **The Hub sees the content of the sessions a Worker shares.** Screens, scrollback you capture, text you type, the messages you send an agent and the replies recorded from it pass through the Hub and, for the pipe, are stored in its database. A Hub operator, and anyone who can read the Hub's disk or a backup, can read them. Run the Hub on a machine you control, and share only the sessions you are willing to show it ([pairing and sharing](guides/pairing.md#choosing-what-the-hub-sees)).
- **A proxy that ends TLS for the web UI is trusted with all of it.** It sees every page and every event the browser sees. Run it yourself. To keep a proxy out of the content, let it pass the encrypted stream through and give the Hub a certificate of its own ([reverse proxy](guides/reverse-proxy.md)).
- **A session that is not shared is listed by name only**, to its owner and the administrators, so that they can choose to share it. The Hub never shows its content, state, task or history. The Worker enforces this itself, whatever the Hub or a caller asks for. When you stop sharing a session, the Hub stops serving what it stored of it; the stored history is hidden, not erased, and shows again if you share the session again.
- **The Worker's own user decides what is reachable.** A Worker runs as one Unix user (or Windows account) and can do to that user's tmux sessions everything the user can. A Worker in a container that mounts the host user's tmux socket has the same reach ([Docker Worker](install/worker-docker.md#the-trust-boundary)).
- **Roles:** an administrator may do everything, including manage users, Workers and settings; the owner of a Worker may read, operate and message on it; nobody else has any access. An API key never has more than its owner has, and is further limited to the resources and actions it was created with.

## Enrollment is not authorization: new Workers wait for approval

An invitation key, a pairing code or the infrastructure key file lets a machine **enroll**; it does not let that machine be **operated**. Every new Worker starts *pending*: it may connect, but the Hub routes no operation to it (no user, key or other Worker can name it as a target), its calls to the Hub are refused with `worker_pending` (it may still retire itself), and its sessions are not listed or read. The owner of the Worker or an administrator approves it in the Hub after comparing its fingerprint, host name and source address, with `minion hub worker approve`, or through `worker_approve` ([Approve a new Worker](guides/pairing.md#approve-a-new-worker)). So a leaked invitation key alone gives an attacker a pending, inert Worker that you can see and remove, not a way to read or type into anything, and not a Worker that can call other Workers.

What bounds a leaked key further: a user's key lasts at most one week, a user's reusable key allows at most 100 enrollments (10 by default), and the infrastructure key file expires (30 days by default), counts its uses and can be revoked from the Invitations page; reloading the file never resets any of this. Approve only a Worker you recognise: approval is the authorization, and an approved Worker has all the reach a Worker of its owner has.

## Worker isolation: one compromised Worker does not reach the others

A Worker identity can read and message **only its own Worker and that Worker's sessions**. It cannot reach another Worker, not even one of the same owner, unless the owner gives it a **call key**: an API key bound to that Worker with a narrow scope, set as `call_api_key_file` on the Worker ([API keys and MCP](guides/api-keys-and-mcp.md#let-a-worker-call-other-workers)). The receiving Worker then decides for itself with its local setting `pipe_accept_from` (`none` by default, `owner-workers`, or a list of Worker ids or fingerprints) whether it takes messages from another Worker ([the pipe](guides/pipe.md#agent-to-agent-the-call-key-and-pipe_accept_from)). The Hub tells every Worker who made each request (`origin`: user, API key or Worker, with the key and the sending Worker), and a Worker refuses a message-class request that has no valid origin. The setting can be changed only on the Worker itself, and each refusal is in the Worker's local audit.

What this protects, and what it does not:

- It protects every Worker against a **compromised peer Worker, or an agent on it that was talked into misbehaving**, as long as the **Hub is honest**. Enrolling a Worker, or stealing its identity key, no longer gives a way to type into the rest of the fleet.
- It does **not** protect against a compromised Hub or a malicious administrator. The Hub supplies the origin, and a Hub or an administrator can already create sessions and act as users. Trust the Hub as described above.
- Anything that runs as the Worker's own user can read that Worker's call key file and its local configuration (it can also change `pipe_accept_from` there). Run agents of different trust levels as different users or in different containers.

## A message can make an agent run commands

Sending a message to an agent (`pipe_send`, `send_text`, `send_keys`, `answer`, `set_task`) is the same as typing into it. The agent, or the shell it sits in, may act on that text. Anything that can send, and anything a sender can talk into sending, can therefore cause commands to run on that machine, through that agent, with that agent's rights. The same is true of a model that reads a session and then writes to another: what it reads can steer what it types.

Consequences you should draw:

- Give a person, key or model the right to send (`operate`, `message`) only on the Workers and sessions where you accept that.
- The MCP server is **read-only by default**; the tools that type, create sessions, send messages or change sharing appear only with `--allow-writes` ([API keys and MCP](guides/api-keys-and-mcp.md)).
- Replies recorded from an agent are the agent's output, not a person's words. A model receives received turns only from sources its user has marked as **trusted** ([the pipe](guides/pipe.md)).
- Writes that are guarded (for example, a pane that changed since you looked, a shell prompt where an agent was expected) are refused with a reason and are repeated only when you pass `--force` yourself; nothing forces a write on your behalf.

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

## Pairing and the Hub identity

A Worker pins the Hub's **root key** (its fingerprint) when it pairs or enrolls; later it trusts that key, not any TLS certificate. A proxy, a tunnel or a certificate change therefore does not weaken or break it, and a Hub that cannot prove it holds the pinned root is not trusted. A master-key rotation is a signed chain that Workers follow without pairing again. The operations that touch the root (set the passphrase, re-issue the certificate, rotate the master key, change the passphrase) need the administrator role, a sign-in from the last five minutes, the CSRF token, a same-origin request and an effective HTTPS connection; API keys cannot perform them, and a passphrase is never stored or logged. See [Hub identity](guides/hub-identity.md).

**The pairing handshake (a password-authenticated key exchange over the pairing code) has not had an independent cryptographic review yet.** Until it has, pair on networks and with codes you would also trust for a password prompt, keep the code short-lived (it expires after 10 minutes), and prefer invitation keys created in the Hub for unattended enrollment.

### Two keys: a sealed root and a short-lived operational key

Since 0.7.0 the Hub does not sign its handshakes with the long-lived identity key. There are two keys ([design](architecture/proposals/hub-identity-v2.md)):

- The **root key** is the identity Workers pin. It lives on the Hub only in an encrypted file, `root.key.age`, sealed with a **passphrase** that you type and keep elsewhere. The Hub opens it only for the moment a certificate or a rotation is signed, then wipes it.
- The **operational key** (`op.key`) is plain on the Hub's disk, so the Hub can restart unattended. It signs the handshake. The root certifies it with a **Hub certificate** (153 bytes: both public keys, a serial, a start and an end time, and the root's signature). A certificate is valid for **180 days** by default (1 to 400 when you choose) and **every certificate carries a new operational key**. Nothing renews a certificate by itself, because that needs the passphrase and the Hub never holds it: you re-issue it before it expires, from the Hub identity page or with `minion hub identity reissue` ([Hub identity](guides/hub-identity.md)). The page warns from 30 days before expiry.

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

## Updates

`minion update` installs only a release whose signature identifies this project's release workflow for that exact tag, recorded in the Sigstore transparency log, and whose archive matches the signed checksums. The `bootstrap.sh` installer has two modes, and they are not the same:

- **Signed release (the default):** it verifies the same signature of the checksum list as `minion update` does (with a pinned `cosign`), then the archive against those checksums.
- **Checksum-pinned (an exact version plus `MINION_SHA256`):** it checks only that the archive has the SHA-256 you supplied. It does not fetch or verify the checksums file, the signature or the transparency-log entry, so the signature guarantees above do **not** apply to this mode. Take the checksum from a source you trust independently of the download (for example the signed `checksums.txt` that you verified yourself), because the installer trusts it completely.

 There is no database downgrade: an older Hub refuses a database a newer one changed. `minion update` has a check that refuses a release whose protocol range does not overlap the Hub's, but in this release the source of the connected Hub's range is not wired in production: **without the explicit flag `--hub-proto-range MIN..MAX` (for example `--hub-proto-range 3.0..3.0`) the updater performs no protocol check and installs the release**. An older Worker therefore does not protect itself; follow the upgrade order yourself (Hub first, see [Upgrading from 0.6](#upgrading-from-06)). Release 0.7.1 speaks wire protocol **3.0** only (the signed release manifest carries the range `3.0..3.0`): there is no fallback to 2.x, a Hub and a Worker of an earlier build cannot connect (a 2.0 Worker is answered `proto_incompatible` and retries until it is upgraded), and mixed versions are not supported, so upgrade the Hub and all Workers together.

## When something is not known

- Reads that fail are retried or reported as errors. A write that may have been delivered, but whose answer was lost, is reported as **unknown** and is never replayed for you; you repeat it with the same request id on purpose.
- A change of sharing whose answer was lost is treated as possibly made: the Hub hides what the change would have hidden until the Worker confirms its rule.
- While the Hub does not know a Worker's sharing rule it hides that Worker's stored history instead of showing it.
- Sign-in through an identity provider fails closed: a claim that is missing or matches no configured value gives no role, and no role means no access ([OIDC](guides/oidc.md)).

## Not covered, or not finished

- The Windows Worker is a **preview**: its sessions end with the Worker process, and a Windows service runs as the LocalSystem account ([Windows preview](install/worker-windows-preview.md)).
- The pairing handshake is awaiting independent cryptographic review (above).
- The Hub passphrase has no recovery path (above), and a Worker that stays offline for the whole remaining life of a stolen certificate is not protected by a re-issue.
- A change of sharing made on the Worker's machine reaches the Hub within the Worker's poll interval (one second by default). Changes made through the Hub apply before the Hub answers.
- Local users of the Worker's machine are not an adversary the Worker defends against: the control socket and the state directory are private to the Worker's user, and anything running as that user is already inside the boundary.

Do not put the details of a suspected vulnerability in a public issue; use the repository's private security reporting (GitHub security advisories) when it is enabled.
