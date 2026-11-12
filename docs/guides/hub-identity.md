# Hub identity: passphrase, certificate and master key

Back to the [README](../../README.md). The design is in [hub-identity-v2](../architecture/proposals/hub-identity-v2.md); this page is how to operate it.

The Hub has a **root key** that Workers pin, sealed in `root.key.age` under a **passphrase**, and a **certificate** for an operational key that the Hub uses while it runs. The Hub never holds the passphrase: every change asks for it, and a restart does not.

| State | Meaning | What to do |
| --- | --- | --- |
| B0 | New Hub, nothing created yet. Workers cannot connect. | **Set passphrase**. |
| B1 | Hub migrated from 0.6, the root is still a plain file. Workers cannot connect. | **Set passphrase** (the fingerprint does not change). |
| B2 | A sealed root exists, there is no certificate. | **Re-issue certificate** with the existing passphrase. |
| B3 | Established. | Re-issue before the certificate expires. |

**Set passphrase** is accepted only in B0 and B1. In any other state the Hub answers `conflict` and reads and counts nothing.

## The page

### Passphrase rules

These rules apply to **Set passphrase**, **Change passphrase** and **Rotate master key** (the new passphrase), on the page, over the API and in the commands. The page shows them before each of the three forms.

- At least **14 characters**, counted as Unicode code points (not bytes, not UTF-16 units). The page counts them as you type.
- At most **1024 bytes** in UTF-8. A character takes 1 to 4 bytes, so a very long passphrase of non-Latin letters or emoji can reach the limit before 1024 characters.
- **Recommended:** a sentence of four or more unrelated words, or a long random string from a password manager. It cannot be recovered; keep it where the Hub's owner can find it. Use a passphrase you use nowhere else; one that equals your login password is refused.
- **Unicode entry:** any Unicode characters are accepted, including spaces and accented letters. The Hub does not normalize the text, so type it the same way every time: another keyboard layout or input method can produce different code points for what looks like the same text.

Administration, **Hub identity** (administrators only) shows the state, the root fingerprint, the certificate serial, validity and the days left, when the last change was made, and the lockout state with the time left. The four buttons are **Set passphrase**, **Re-issue certificate** (validity 1 to 400 days, default 180; an optional serial floor under Advanced), **Rotate master key** (you type `ROTATE`; all invitations are revoked, Workers follow the chain without pairing again) and **Change passphrase**. The page never shows a passphrase again: the fields are cleared after every submission, and no response, log line or audit event contains one.

Every operation needs, in this order: the administrator role; a **sign-in from the last five minutes**; the passphrase in the request body; the CSRF token and a same-origin request (`Origin`, and `Sec-Fetch-Site` when sent) with a JSON body; a connection that is **HTTPS** by the rule below; then the passphrase lockout and the allowance (10 web operations per hour). One operation runs at a time; a second waits up to 5 seconds and then answers `conflict`.

### Confirm it is you

The page has a "Confirm it is you" panel. A session that signed in with a local password re-enters the password (`POST /auth/reauth`). A session that signed in through an identity provider is sent to the provider again with `max_age=0` and `prompt=login`; the callback must carry an `auth_time` not earlier than the start of the request (60 seconds of clock allowance; it must also be no older than the five-minute window and not more than 60 seconds ahead of the Hub's clock). The session records that `auth_time`, not the time of the callback and the same subject as the session. If the provider does not return `auth_time`, ignores `max_age`, returns another subject, or the state is unknown, expired or already used, the answer is `reauth_unavailable` and nothing changes. A new session, a silent single sign-on redirect or a refreshed token is not a fresh sign-in. Nothing extends the five minutes except a new check. A failed re-authentication is audited as `hub_identity.reauth_failed` and is not a passphrase failure.

### The HTTPS rule

A passphrase is accepted only when the request's effective scheme is HTTPS: the Hub's own TLS, or `X-Forwarded-Proto: https` from a peer inside `MINION_TRUSTED_PROXIES`. A loopback address is not by itself a reason to accept cleartext, and forwarded headers from any other peer are ignored. Otherwise the answer is `tls_required`. The only exception is the **management listener**: set `MINION_MANAGEMENT_LISTEN=127.0.0.1:8444` and the Hub also serves plain HTTP on that loopback address, for an operator who works on the Hub host (for example through an SSH tunnel). Do not point a reverse proxy at it.

## REST

`hub_identity_status`, `hub_identity_set_passphrase`, `hub_identity_reissue`, `hub_identity_rotate` and `hub_identity_change_passphrase` are operations of the catalog (`POST /api/v1/ops/<name>`, JSON). An administrator holds them; **API keys and act-as sessions cannot perform the four that take a passphrase** (`forbidden`); the status can be read by an administrator or by an everything key. Errors: `reauth_required` (sign in again; details name the method), `reauth_unavailable`, `tls_required`, `passphrase_invalid` (one generic message for a wrong passphrase, a damaged file or an unsupported one), `rate_limited` (details: `limit` is `locked` or `allowance`, and `retry_at`), `conflict`, `invalid_argument`. The generic `minion <op>` commands do not exist for the four operations: the passphrase is asked on a terminal.

## Commands at the Hub host

```sh
minion hub identity status [--json]
minion hub identity set-passphrase
minion hub identity reissue [--validity DAYS] [--serial-floor N] [--allow-far-floor]
minion hub identity rotate-master          # also: rotate
minion hub identity change-passphrase
```

They run as the Hub's user (or root) and talk to the running Hub over the **local admin socket** `admin.sock` in the data directory (mode 0600). They ask for the passphrase on the terminal with echo off and **refuse to run when standard input is not a terminal**; there is no flag, variable or file for the passphrase. The Hub takes the actor from the kernel credentials of the socket peer (`local:<uid>`), not from the client. The fresh sign-in, CSRF and HTTPS rules of the web do not apply, because a browser cannot reach the socket. The commands have their **own** failure counter, lockout and hourly allowance, so a lockout caused through the web never locks the owner out at the Hub host. `minion hub identity` without a subcommand still prints the root fingerprint; it needs no running Hub, no private key and no socket (once the key file is gone it reads the public fingerprint from the database, read-only). root may run the commands for the Hub account: the client checks that the socket is owned by the owner of the data directory and is not group- or other-writable. Ctrl+C or SIGTERM at a prompt restores the terminal (echo on) and ends the command with exit status 130.

Exit statuses: 0 done; 1 other error; 2 usage (including no terminal); 3 the state does not allow it, or the change must be repeated; 5 passphrase not accepted; 6 locked or allowance used up; 7 no running Hub answers on the socket.

## Lockout

After 5 wrong passphrases in a scope the scope is locked (15, 30, 60, 120, then 240 minutes) and the page shows the time left. A request refused because of a lock consumes no allowance. The lock and the counters survive a restart. The lock does not stop a restart of the Hub, Workers that are already connected, or the other scope.

## Expiry

Nothing renews a certificate by itself, because renewing needs the passphrase. From 30 days before `not_after` administrators see a banner on every page (red from 7 days), the Workers page shows the countdown, the Hub logs a line at start and every 24 hours, and the audit event `hub_identity.expiry_warning` is written once per UTC day. Besides the daily check the Hub sets a timer for the `not_after` of the current certificate, so the expiry (closing Worker sessions, `hub_identity.expired`) happens at that moment and not up to a day later; every issuance or rotation arms it again. At expiry the Hub closes Worker sessions, refuses new Worker connections and writes `hub_identity.expired`; the web UI, the API and the commands keep working so you can re-issue.

## Audit events

`hub_identity.passphrase_set`, `.passphrase_change`, `.reissue`, `.rotate`, `.reauth_failed`, `.unseal_failed`, `.locked`, `.expiry_warning`, `.expired`. Each has the actor (the user id for the web, `local:<uid>` for a command, `system:identity-expiry` for the timer) and the channel (`ui`, `cli`, `system`). A change and its event are written in one transaction.
