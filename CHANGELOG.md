# Changelog

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
