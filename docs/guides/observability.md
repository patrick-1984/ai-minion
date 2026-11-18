# Log export: syslog and OpenTelemetry

Back to the [README](../../README.md).

The Hub and every Worker write their log to standard error (and the Worker keeps its recent lines in memory, see [troubleshooting](troubleshooting.md)). Since v0.14.0 each of them can also send its log records to a **syslog server** and to an **OpenTelemetry collector**, each with its own switch, endpoint and level. This page covers both, how secrets are kept out of them, and how to see what a sink is doing.

- [What leaves the machine](#what-leaves-the-machine)
- [Levels](#levels)
- [Syslog](#syslog)
- [OpenTelemetry](#opentelemetry)
- [Hub: settings page, environment and live changes](#hub-settings-page-environment-and-live-changes)
- [Worker: flags, environment and the service](#worker-flags-environment-and-the-service)
- [Status and problems](#status-and-problems)
- [A collector example](#a-collector-example)
- [Settings reference](#settings-reference)

## What leaves the machine

A record goes to a remote sink only after it was cleaned, in this order:

1. **Human notices stay local.** Two records carry a working credential on purpose, for the person at the console: the generated initial admin password of a new Hub and the pairing code of a Worker. They keep their level and text on standard error (and `docker logs`); no remote sink ever receives them.
2. **Known secrets are masked** in the message and in every attribute (also attributes bound to a logger and members of groups): values of secret attribute names (`password`, `token`, `key`, `authorization`, `cookie`, `api_key`, ...), the user information and the query of every URL, pairing codes `MN-XXXX-XXXX` (with or without dashes, any case), and Minion's own secret formats: API keys `mn_...`, invitation keys `mnk_<fingerprint>.<secret>` (the fingerprint stays) and join tokens `mnj1-...`. Errors are rendered with URLs cut to scheme and host. A value that is not text (a struct, a map) is turned into JSON first; members with a secret name are masked at any depth, then the text is masked like any other text. Invalid UTF-8 is replaced (a collector rejects a whole batch for one invalid string).
3. **Sizes are capped**: the message at 4 KiB, one text attribute at 2 KiB, the whole record at 16 KiB, cut on a character boundary with `…[truncated N bytes]`.

What is **not** masked: a secret that does not have one of these exact forms, for example someone else's password inside a free-text error, or a pairing code typed as eight characters without `MN`. Minion does not guess secrets from their look. If your logs can carry such text, mask it at the collector (the [example](#a-collector-example) has a `transform` and a `redaction` processor for that).

The rule of the local log still holds: Minion never logs terminal content, message bodies, passwords, keys or secrets.

## Levels

Every sink has its own level, independent of `log_level`. The levels are the same four as everywhere: `debug`, `info`, `warn`, `error` (default `info`). A record is built once and handed to every destination whose level it meets, so a debug record costs nothing when no destination wants debug.

| Process | Standard error (and the Worker's in-memory log) | `syslog_level`, `otel_level` | Remote debug (`worker_debug`) |
| --- | --- | --- | --- |
| Hub (`minion hub serve`) | `log_level`: environment, then the panel, then `-v` / `verbose`, then `info`. **The panel value applies at once** (before v0.14.0 a `log_level` stored in the panel did nothing). | per sink: environment, then the panel, then `info` | not available on the Hub |
| Worker daemon (`worker run`, the service) | as before: `--debug`, then `log_level`, then `-v`, then `verbose`, then `info`; flag, then environment, then the config file | per sink: flag, then environment, then the config file, then `info` | lowers standard error and the in-memory log only; the sinks keep their level, so a Hub administrator cannot raise what leaves the Worker machine |
| One-shot commands (`minion <op>`, `minion mcp`, `worker pair`) | as before | never exported | not available |

## Syslog

Turn it on with `syslog=on` and an endpoint:

| Endpoint | Transport |
| --- | --- |
| `udp://host:514` | RFC 5426: one message per datagram. Lossy, and a datagram larger than the path allows is dropped by the network. |
| `tcp://host:601` | RFC 6587 with octet counting (`<length> <message>`). |
| `tls://host:6514` | RFC 5425: TLS 1.2 or newer with octet counting. The server certificate is checked against the system roots or `syslog_ca_file`; `syslog_client_cert_file` and `syslog_client_key_file` add a client certificate (mutual TLS). |

Each message is RFC 5424 with facility `local0`:

```text
<134>1 2026-11-18T20:48:01.123456+01:00 hub-1 minion-hub 4242 workers - ﻿{"time":"2026-11-18T20:48:01.123456789+01:00","level":"INFO","msg":"worker connected","component":"workers","worker_id":"0193..."}
```

- Severity: `debug` 7, `info` 6, `warn` 4, `error` 3. APP-NAME is `minion-hub` or `minion-worker`, PROCID the process id, MSGID the record's `component` (or `-`). Structured data is `-`.
- The message (MSG) starts with the UTF-8 byte order mark and is one JSON object, the same shape as the JSON lines on standard error.
- `syslog_max_bytes` (default 2048, 480 to 65000) is the size of the whole message. A larger record loses its largest attributes first and gets `"truncated":true`; if that is not enough, the message text is cut. The result is always valid JSON. 2048 is what RFC 5425 and RFC 5426 say a receiver should accept and 480 what every IPv4 receiver must accept; use TCP or TLS for large records.
- The receiver must expect octet counting on TCP and TLS: rsyslog `imtcp` with `SupportOctetCountedFraming="on"`, syslog-ng `syslog()` source, or the OpenTelemetry Collector `syslog` receiver with `enable_octet_counting: true`.

Logging never waits for the server: records go into a queue of 1000 that drops the oldest record when it is full. Connecting, the TLS handshake and each write give up after 5 seconds; then the sink reconnects with a growing pause (1 second up to 1 minute, with jitter).

## OpenTelemetry

Turn it on with `otel=on` and an endpoint. Minion sends OTLP over HTTP with protobuf and gzip (the OpenTelemetry default; port 4318), not gRPC.

- `otel_endpoint` is the base URL, for example `http://collector:4318` or `https://otel.example.com`; Minion sends logs to `<base>/v1/logs`.
- Without `otel_endpoint`, the standard variables work: `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` is used **as is** (the full URL), otherwise `OTEL_EXPORTER_OTLP_ENDPOINT` plus `/v1/logs`.
- **Headers** (for example an `Authorization` header or a tenant id) are one `Name: value` per line (names are case-insensitive; a name given twice is refused), in `otel_headers` on the Hub (a secret: stored encrypted, or `MINION_OTEL_HEADERS_FILE`) or in the file named by `otel_headers_path` on a Worker. `OTEL_EXPORTER_OTLP_HEADERS` and the per-signal header variables are **ignored** (with a warning that names only the variable): Minion reads secrets only from files, the panel or its own config, and passes every exporter option itself so that no header value can end up in an error message.
- The endpoint URL must not have a query, a fragment or percent-encoded path characters. Minion does not follow redirects (a `307` or `308` would send the batch and its headers to another address), and it never shows what a collector sends back with an error: the status and the diagnostics name the HTTP status code or the connection problem only.
- `https://` uses the system roots or `otel_ca_file`; `otel_client_cert_file` and `otel_client_key_file` add a client certificate. `http://` to another machine works but sends records and headers unencrypted; the Hub warns at start.
- Each record carries the resource `service.name` (`minion-hub` or `minion-worker`), `service.version`, `service.instance.id` (the Hub keeps a stable id in its data directory; an enrolled Worker uses its Worker id, which is also `minion.worker.id`; a Worker that is not enrolled yet uses a new id per start) and `host.name`. `OTEL_RESOURCE_ATTRIBUTES` and `OTEL_SERVICE_NAME` are honoured.
- Records are sent in batches (up to 256 every second, queue 1024; a record that finds the queue full is dropped and counted as `queue_full`). A request times out after 5 seconds and is retried for at most 30 seconds.

## Hub: settings page, environment and live changes

On the Hub, open **Administration > Hub settings > Log export**. The section is saved as a whole with **Save section**: the Hub checks the switch, endpoint, headers, certificate files and level together and refuses a combination that cannot work (for example `syslog=on` without an endpoint, or a CA file with a `udp://` endpoint) before anything is stored. A saved change applies at once, without a restart: the Hub builds the new sink, switches to it and drops what the old sink had not sent yet, so that no record queued for the old endpoint or credential is sent with the new one (an export in progress is cancelled). If someone else changed a setting of the section after you loaded the page, the save is refused; reload the page. If a saved configuration cannot be applied (for example a key file disappeared), both sinks are switched off and the section shows why; this also applies at Hub start, which never fails because of a sink setting. A save that keeps the stored headers also checks that they did not change since the page was loaded. The header field takes several lines; leave it empty to keep the stored headers.

When the headers or a client key come from the environment, the endpoint of that sink must come from the environment too: the panel cannot send a deployer's credential to another destination.

A setting set by an environment variable (`MINION_SYSLOG`, `MINION_OTEL_ENDPOINT`, ...) is read-only in the panel, as every Hub setting. The certificate settings take paths, set with `MINION_SYSLOG_CA_PATH`, `MINION_OTEL_CLIENT_KEY_PATH` and so on (variables that end in `_FILE` are reserved for secrets). A key file must be readable by its owner only (mode 0600).

Through the API or the CLI, `settings_set` takes a batch:

```sh
minion settings_set --changes '[{"key":"otel","value":"on"},{"key":"otel_endpoint","value":"https://otel.example.com"}]'
minion settings_set --changes - < rotation.json   # a batch with otel_headers: standard input only
```

A Docker example that sends the Hub's log to a collector in the same Compose project:

```yaml
environment:
  MINION_OTEL: "on"
  MINION_OTEL_ENDPOINT: http://otel-collector:4318
  MINION_OTEL_HEADERS_FILE: /run/secrets/otel_headers
```

## Worker: flags, environment and the service

The same keys exist on every Worker as flags, variables and config-file keys:

```sh
minion worker up --syslog on --syslog-endpoint tls://logs.example.com:6514 --syslog-level warn
minion worker up --otel on --otel-endpoint https://otel.example.com --otel-headers-path /home/me/.config/minion/otel-headers
```

`worker up` saves them in the Worker's config file like every other setting, so the installed service (systemd, launchd, Windows service) uses them; the header file stays where it is and only its path is saved (a relative path is saved absolute). Before it saves anything or touches the service, `worker up` checks the sinks as the service will see them: the service does not see `OTEL_EXPORTER_OTLP_*` variables of your shell, so use `--otel-endpoint`. The file must be a regular file of the Worker's user that nobody else can read (mode 0600 on Linux and macOS). `minion worker config set otel_level debug` changes one setting; restart the Worker to apply it. A sink setting that does not validate does not stop the Worker: both sinks stay off and `minion worker status` and the Worker's log name the setting. `worker up` refuses such a setting before it saves it, and checks the header and key files with the Worker's own rules (a regular file of the service's account, not a link, mode 0600); a relative path already in the config file is refused.

## Status and problems

- **Hub**: Administration > Diagnostics and the Log export section of Hub settings show the effective state of each sink; `minion hub_diagnostics` lists it as `sinks`: name, on or off, destination (scheme, host and port only), level, a generation number that grows with each change, last success, last error, queued, sent, and the number of records dropped by reason (`queue_full`, `write_failed`, `export_failed`, `reconfigure`, `shutdown`).
- **Worker**: `minion worker status` shows one line per sink, `--json` the same fields under `log_sinks`.
- A sink that cannot reach its server reports it on standard error only, at most once a minute per kind of problem (with the number of suppressed repeats), never to a remote sink.
- On stop, the Hub gives its sinks up to 3 seconds and a Worker up to 2 seconds to send what is queued; what is left is dropped and the counts are written once to standard error.

## A collector example

[`docs/examples/otelcol-minion.yaml`](../examples/otelcol-minion.yaml) is a configuration for the OpenTelemetry Collector (contrib distribution) that receives OTLP/HTTP on port 4318 and RFC 5424 syslog over TCP with octet counting on port 6514 (TLS commented out), masks bearer tokens and passwords in bodies (`transform`, OTTL `replace_pattern`) and attributes (`redaction`, `blocked_values`), and prints what arrives (`debug` exporter):

```sh
docker run --rm -p 4318:4318 -p 6514:6514 -v "$PWD/docs/examples/otelcol-minion.yaml:/etc/otelcol-contrib/config.yaml:ro" otel/opentelemetry-collector-contrib:0.165.0
```

Point Minion at it with `otel_endpoint=http://<host>:4318` and `syslog_endpoint=tcp://<host>:6514`.

## Settings reference

H = Hub (environment, then the panel; live), W = Worker (flag, then environment, then config file; at restart).

| Key | Where | Values | Default |
| --- | --- | --- | --- |
| `syslog` | H, W | `off`, `on` | `off` |
| `syslog_endpoint` | H, W | `udp://host:port`, `tcp://host:port`, `tls://host:port` | none (needed when on) |
| `syslog_level` | H, W | `debug`, `info`, `warn`, `error` | `info` |
| `syslog_max_bytes` | H, W | 480 to 65000 | 2048 |
| `syslog_ca_file` (`MINION_SYSLOG_CA_PATH`) | H, W | PEM file | system roots |
| `syslog_client_cert_file`, `syslog_client_key_file` (`MINION_SYSLOG_CLIENT_CERT_PATH`, `MINION_SYSLOG_CLIENT_KEY_PATH`) | H, W | PEM files, key mode 0600 | none |
| `otel` | H, W | `off`, `on` | `off` |
| `otel_endpoint` | H, W | OTLP/HTTP base URL | none; then `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `otel_level` | H, W | `debug`, `info`, `warn`, `error` | `info` |
| `otel_headers` | H | secret, `Name: value` lines (`MINION_OTEL_HEADERS_FILE`) | none |
| `otel_headers_path` | W | path of a 0600 file with `Name: value` lines | none |
| `otel_ca_file`, `otel_client_cert_file`, `otel_client_key_file` (`MINION_OTEL_CA_PATH`, `MINION_OTEL_CLIENT_CERT_PATH`, `MINION_OTEL_CLIENT_KEY_PATH`) | H, W | PEM files, key mode 0600 | system roots / none |

Fixed: facility `local0`; OTLP timeout 5 s and gzip; the queue and batch sizes above.
