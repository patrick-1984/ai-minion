# Troubleshooting: verbose logs and diagnostics

Back to the [README](../../README.md).

When a Worker does not connect, a page behind a reverse proxy refuses a sign-in or an operation is refused, three tools show what the Hub and the Worker see.

## Verbose logs: `-v` and `MINION_VERBOSE`

Every `minion` (and `mn`) command accepts `-v` (or `--verbose`). It sets the log level to `debug`. The environment variable `MINION_VERBOSE=1` does the same, for example in a Compose file or a service. An explicit level wins: `--log-level` or `MINION_LOG_LEVEL` (and `log_level` in the Worker's config file) override `-v` and `MINION_VERBOSE`.

```sh
minion worker run -v
MINION_VERBOSE=1 minion hub serve
```

At debug level each of the following is one structured log line:

- **Worker**: each dial of the Hub (attempt, enrollment state, Worker id), the handshake result, the welcome (including whether the Worker waits for approval), why a connection ended and how long it lasted, and each backoff delay.
- **Hub**: each HTTP request with the scheme decision (direct peer, whether it is a trusted proxy, client IP, the forwarding headers seen, effective scheme and HTTPS), each refused API request with the stage (authenticate, arguments, authorize) and the error code, each operation routed to a Worker (op, Worker, outcome, duration) and each call stream from a Worker (op, credential, outcome).
- **Op commands** (`minion sessions_list` and the others): the op, the backend (Hub or local Worker), the outcome and the duration.

Debug lines never contain keys, passphrases, pairing codes, API keys, terminal content or operation arguments, and the Hub logs request paths without their query.

`minion worker up -v` saves the verbose setting to the Worker's config file like any other setting, so the installed service logs at debug level too. Run `minion worker up --verbose=false` to turn it off again.

## Worker debug: `--debug` and `MINION_DEBUG`

`minion worker up --debug` (or `minion worker run --debug`, or `MINION_DEBUG=1`) runs the Worker at debug level. It is the Worker's own switch for `log_level=debug` and wins over every other level. Besides the lines above, a debug Worker logs:

- **Session host** (Windows): the `session_host` mode, whether the Worker runs under a service manager, where psmux was found (`PATH`, the WinGet links directory, or where it looked), the version psmux printed and why the Worker fell back to ConPTY.
- **Session listings**: the command, whether it worked, the number of sessions and, for psmux, the sessions left out because psmux cannot address their names. A listing that repeats the previous result is not written again.
- **Share filter**: how many sessions were listed, how many are shared and the share mode (`later` shares nothing until you decide).
- **Event stream**: the snapshot sent to the Hub and each batch of session changes.
- **Self-update**: from and to, why an update was refused, the installer steps, and which restart path the Worker takes (service, console, exec or detached).
- **Hub connection**: the scheme and host of the WebSocket URL and of the proxy, the HTTP status, the TLS version and the Hub certificate's subject, issuer and SHA-256 fingerprint, the handshake result, the negotiated protocol and the certificate's end.

`minion worker up --debug` saves the switch to the config file, so the service logs at debug level too; `minion worker up --debug=false` turns it off.

## Remote debug from the Hub: `--allow-debug-remote`

A Worker keeps its most recent log lines in memory (2000 lines or 512 KB). When it allows it, the Worker's owner or an administrator can switch its debug logging on for a while and read those lines through the Hub, without access to the machine. It is off by default; allow it on the Worker machine with one of:

```sh
minion worker up --foreground --allow-debug-remote     # with your usual flags
minion worker config set allow_debug_remote true       # for the service; then restart the Worker
MINION_ALLOW_DEBUG_REMOTE=1 minion worker up --foreground
```

Then use the **Debug** panel on the Worker's page in the web UI (turn debug on for 1 to 1440 minutes, default 60; it switches itself off after that; read the recent log), or the operations through the Hub:

```sh
minion worker_debug --worker-id <id> --enabled --minutes 30
minion worker_logs --worker-id <id> --lines 500
minion worker_debug --worker-id <id> --enabled=false
```

A Worker that does not allow it refuses both with the command to allow it. Both work on a preview-only Worker too: they touch no session. Every call, the panel's reads included, is recorded in the Hub's audit log. The log never holds terminal content, messages, keys or pairing codes: secret values, pairing codes and the credentials and query of URLs are masked before a line is kept, and the Hub connection lines show only the scheme and host of the Hub and the proxy. When the Worker is paired again, moves to another owner or is removed, it clears the kept lines and switches remote debug off.

## Worker status as JSON

```sh
minion worker status --json
```

prints the Worker's state for scripts and bug reports: `hub_url`, `paired` (enrolled with a Hub), `worker_id`, `approval` (`approved`, `pending` or `unknown`) and `connection` with `state`, `reason`, `last_error` and `last_error_at`. The connection, the approval and the pairing code are live data: they are known only while the Worker runs, otherwise they say `unknown`. Since 0.11.0 it also has `hub_fingerprint_words`, `worker_fingerprint_words`, `hub_check`, `hub_check_at`, `hub_check_anchor`, `hub_check_text`, `alarm` and `tls` (`state`, `line`).

Without `--json` the rows about the Hub are:

| Row | Shows |
| --- | --- |
| Hub fingerprint, Hub fingerprint (full) | the pinned Hub key as six words and as 52 characters |
| Hub check | `not verified`, `matched the fingerprint you gave`, `from the invitation key` or `DOES NOT MATCH the fingerprint you gave - not connecting`; after a master-key rotation it adds `; the Hub key was reached by verified rotation from <words>` |
| Hub identity | `CHANGED - not connecting`, only while that alarm is on |
| TLS | the [TLS path](reverse-proxy.md#what-a-worker-reports-behind-a-proxy) and the certificate the running Worker saw |
| Worker fingerprint, Worker fingerprint (full) | this Worker's key, to compare when you approve it |

## Hub fingerprint messages

- **`not verified`**: the Worker paired on first use without an expected fingerprint. Compare all six words with `minion hub identity --words` on the Hub host, then run `minion worker verify-hub <the 52 characters>` on the Worker ([pairing](pairing.md#check-a-paired-worker-verify-hub)).
- **HUB FINGERPRINT DOES NOT MATCH - NOT PAIRED**: at pairing another Hub answered than the one you named. Nothing was stored. Check the value (read it on the Hub host) and the address, and remove a pending Worker in the Hub that does not show this Worker's fingerprint.
- **HUB FINGERPRINT DOES NOT MATCH - NOT CONNECTING**: `verify-hub` or the setting `hub_fingerprint` names another Hub than the paired one. The Worker stays disconnected, also after restarts (box **WORKER QUARANTINED** at later starts); only `verify-hub <52 characters>` of the paired key or `--forget` ends it. If the value is right, pair again with `minion worker pair --new-code --hub-fingerprint <value>` and remove the old Worker in the Hub. If the value was wrong, run `verify-hub` with the right one, or remove the pairing with `minion worker verify-hub --forget --pinned <52 characters>`.
- **HUB IDENTITY CHANGED - NOT CONNECTING**: a paired Worker got a Hub key it does not accept (another key, or a master-key rotation record that does not verify). It keeps its pairing, keeps retrying and does not connect; the box is printed once, and the log has the error `hub identity changed; not connecting`. Read `minion hub identity --words` on the Hub host. If it shows the "paired with" words, something between the Worker and the Hub answers in its place: check the proxy and the network, and do nothing on the Worker. If the Hub was reinstalled or restored with another identity, pair again with `minion worker pair --new-code --hub-fingerprint <value>`.
- **A malformed `hub_fingerprint`** stops the Worker at start. Give six words or the 52 characters, or unset it.

## Hub diagnostics

Administrators open **Administration > Diagnostics** in the web UI. The page shows the Hub's version and commit, when it started and its uptime, the public URL and trusted-proxy settings, how many Workers are connected and how many of them wait for approval, and how the Hub sees your own browser request: the peer address, whether the peer is a trusted proxy, the client IP it resolves, the forwarding headers received (honoured only from a trusted proxy), the effective scheme and whether the request counts as HTTPS.

The same facts come from the operation `hub_diagnostics` (admin only), with the command line against the Hub ([API keys](api-keys-and-mcp.md)):

```sh
minion hub_diagnostics --json
```

If the peer is your proxy but "trusted proxy" says no, add its address to `MINION_TRUSTED_PROXIES` ([reverse proxy](reverse-proxy.md)).
