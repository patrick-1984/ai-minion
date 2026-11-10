# Security and trust model

Back to the [README](../README.md).

Minion lets people, scripts and models read and type into terminal sessions on other machines. That is the product, and it defines what you are trusting. This page states the boundaries as they are in release 0.5.0, including what has not been reviewed yet.

## Who sees what

- **The Hub sees the content of the sessions a Worker shares.** Screens, scrollback you capture, text you type, the messages you send an agent and the replies recorded from it pass through the Hub and, for the pipe, are stored in its database. A Hub operator, and anyone who can read the Hub's disk or a backup, can read them. Run the Hub on a machine you control, and share only the sessions you are willing to show it ([pairing and sharing](guides/pairing.md#choosing-what-the-hub-sees)).
- **A proxy that ends TLS for the web UI is trusted with all of it.** It sees every page and every event the browser sees. Run it yourself. To keep a proxy out of the content, let it pass the encrypted stream through and give the Hub a certificate of its own ([reverse proxy](guides/reverse-proxy.md)).
- **A session that is not shared is listed by name only**, to its owner and the administrators, so that they can choose to share it. The Hub never shows its content, state, task or history. The Worker enforces this itself, whatever the Hub or a caller asks for. When you stop sharing a session, the Hub stops serving what it stored of it; the stored history is hidden, not erased, and shows again if you share the session again.
- **The Worker's own user decides what is reachable.** A Worker runs as one Unix user (or Windows account) and can do to that user's tmux sessions everything the user can. A Worker in a container that mounts the host user's tmux socket has the same reach ([Docker Worker](install/worker-docker.md#the-trust-boundary)).
- **Roles:** an administrator may do everything, including manage users, Workers and settings; the owner of a Worker may read, operate and message on it; nobody else has any access. An API key never has more than its owner has, and is further limited to the resources and actions it was created with.

## A message can make an agent run commands

Sending a message to an agent (`pipe_send`, `send_text`, `send_keys`, `answer`) is the same as typing into it. The agent, or the shell it sits in, may act on that text. Anything that can send, and anything a sender can talk into sending, can therefore cause commands to run on that machine, through that agent, with that agent's rights. The same is true of a model that reads a session and then writes to another: what it reads can steer what it types.

Consequences you should draw:

- Give a person, key or model the right to send (`operate`, `message`) only on the Workers and sessions where you accept that.
- The MCP server is **read-only by default**; the tools that type, create sessions, send messages or change sharing appear only with `--allow-writes` ([API keys and MCP](guides/api-keys-and-mcp.md)).
- Replies recorded from an agent are the agent's output, not a person's words. A model receives received turns only from sources its user has marked as **trusted** ([the pipe](guides/pipe.md)).
- Writes that are guarded (for example, a pane that changed since you looked, a shell prompt where an agent was expected) are refused with a reason and are repeated only when you pass `--force` yourself; nothing forces a write on your behalf.

## Credentials

- **Invitation keys** (`mnk_...`) enroll Workers. **API keys** (`mn_...`) call the Hub. Both are shown once; keep them in files that only their user can read. A key given **literally on a command line is visible in the process list** of that machine to every local user; use a file (`--invite-key <file>`, `MINION_INVITE_KEY_FILE`, `MINION_API_KEY_FILE`). The API key file is checked: a regular file, owned by you, closed to group and others, not a symbolic link. A key left on disk is a credential until it expires or you revoke it.
- Secrets are never accepted as command-line values and are redacted in logs. The one exception is the invitation key, which is meant for one-command setup and carries the warning above. A `curl` call to the Hub's HTTP API must not carry the key in its arguments either: give it the header on standard input ([API keys and MCP](guides/api-keys-and-mcp.md)).
- **Backups and release caches are credentials too.** A Hub backup contains the data key and the Hub identity key ([backup and restore](guides/backup-restore.md)).
- **Two secrets are printed on purpose, and nothing else is.** Protect the logs that hold them (the Worker's console and log, and the Hub's first-start log), and do not paste them into tickets or chat.
  - A **pairing code** (`MN-XXXX-XXXX`) is valid for 10 minutes and is printed on the Worker's console and in its log.
  - A **generated administrator password**: when the Hub starts for the first time without `MINION_ADMIN_PASSWORD_FILE` (or `MINION_ADMIN_PASSWORD`), it makes a random password for the first administrator and logs it once, at error level, so that no log threshold hides it. It must be changed at the first sign-in, but until then the log line is a working credential. To avoid it, give the Hub a private password file (a regular file only the Hub's user can read, mode 0400) as in the [binary](install/hub-binary.md#run-it) and [Docker](install/hub-docker.md#prepare-and-start) guides; a configured password is never logged. If the password was generated, change it, then remove or rotate the log that shows it.

## Pairing and the Hub identity

A Worker pins the Hub's **identity key** when it pairs or enrolls; later it trusts that key, not any TLS certificate. A proxy, a tunnel or a certificate change therefore does not weaken or break it, and a Hub that cannot present the pinned identity is not trusted. A Hub identity can be rotated, and Workers follow the signed chain without pairing again.

**The pairing handshake (a password-authenticated key exchange over the pairing code) has not had an independent cryptographic review yet.** Until it has, pair on networks and with codes you would also trust for a password prompt, keep the code short-lived (it expires after 10 minutes), and prefer invitation keys created in the Hub for unattended enrollment.

## Updates

`minion update` installs only a release whose signature identifies this project's release workflow for that exact tag, recorded in the Sigstore transparency log, and whose archive matches the signed checksums. The `bootstrap.sh` installer has two modes, and they are not the same:

- **Signed release (the default):** it verifies the same signature of the checksum list as `minion update` does (with a pinned `cosign`), then the archive against those checksums.
- **Checksum-pinned (an exact version plus `MINION_SHA256`):** it checks only that the archive has the SHA-256 you supplied. It does not fetch or verify the checksums file, the signature or the transparency-log entry, so the signature guarantees above do **not** apply to this mode. Take the checksum from a source you trust independently of the download (for example the signed `checksums.txt` that you verified yourself), because the installer trusts it completely.

 There is no database downgrade: an older Hub refuses a database a newer one changed. A Worker whose protocol range does not overlap its Hub's refuses an update that would break the connection.

## When something is not known

- Reads that fail are retried or reported as errors. A write that may have been delivered, but whose answer was lost, is reported as **unknown** and is never replayed for you; you repeat it with the same request id on purpose.
- A change of sharing whose answer was lost is treated as possibly made: the Hub hides what the change would have hidden until the Worker confirms its rule.
- While the Hub does not know a Worker's sharing rule it hides that Worker's stored history instead of showing it.
- Sign-in through an identity provider fails closed: a claim that is missing or matches no configured value gives no role, and no role means no access ([OIDC](guides/oidc.md)).

## Not covered, or not finished

- The Windows Worker is a **preview**: its sessions end with the Worker process, and a Windows service runs as the LocalSystem account ([Windows preview](install/worker-windows-preview.md)).
- The pairing handshake is awaiting independent cryptographic review (above).
- A change of sharing made on the Worker's machine reaches the Hub within the Worker's poll interval (one second by default). Changes made through the Hub apply before the Hub answers.
- Local users of the Worker's machine are not an adversary the Worker defends against: the control socket and the state directory are private to the Worker's user, and anything running as that user is already inside the boundary.

Do not put the details of a suspected vulnerability in a public issue; use the repository's private security reporting (GitHub security advisories) when it is enabled.
