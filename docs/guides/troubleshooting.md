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
- **Hub connection**: the WebSocket URL, the proxy, the HTTP status, the TLS version and the Hub certificate's subject, issuer and SHA-256 fingerprint, the handshake result, the negotiated protocol and the certificate's end.

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

A Worker that does not allow it refuses both with the command to allow it. Every call, the panel's reads included, is recorded in the Hub's audit log. The log never holds terminal content, messages, keys or pairing codes.

## Worker status as JSON

```sh
minion worker status --json
```

prints the Worker's state for scripts and bug reports: `hub_url`, `paired` (enrolled with a Hub), `worker_id`, `approval` (`approved`, `pending` or `unknown`) and `connection` with `state`, `reason`, `last_error` and `last_error_at`. The connection, the approval and the pairing code are live data: they are known only while the Worker runs, otherwise they say `unknown`.

## Hub diagnostics

Administrators open **Administration > Diagnostics** in the web UI. The page shows the Hub's version and commit, when it started and its uptime, the public URL and trusted-proxy settings, how many Workers are connected and how many of them wait for approval, and how the Hub sees your own browser request: the peer address, whether the peer is a trusted proxy, the client IP it resolves, the forwarding headers received (honoured only from a trusted proxy), the effective scheme and whether the request counts as HTTPS.

The same facts come from the operation `hub_diagnostics` (admin only), with the command line against the Hub ([API keys](api-keys-and-mcp.md)):

```sh
minion hub_diagnostics --json
```

If the peer is your proxy but "trusted proxy" says no, add its address to `MINION_TRUSTED_PROXIES` ([reverse proxy](reverse-proxy.md)).
