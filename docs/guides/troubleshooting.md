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
