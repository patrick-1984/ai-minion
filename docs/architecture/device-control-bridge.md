# Device control: Worker side and bridge protocol

Status: compiled in, **off**. Device control (on-device adb of the Android Worker) ships disabled until the
device release gate passes (DESIGN section 9, `docs/minion-android-adb/DESIGN.md` of the planning workspace):
the app reports `feature_enabled` only in a build made with `-PminionDeviceControl=true`, and the `adb_*` ops
are `Unreleased` in the catalog (no MCP tool, hidden generic CLI commands, `x-minion-unreleased` in the
OpenAPI). Nothing is reachable by default.

```
 pane (app uid)            Worker (Go, libminion.so)                    App (Kotlin AdbBridge)        adbd
 "$MINION_BINARY" adb ...  worker.sock  -> adb_* ops                    r/minion/adb-bridge.sock  TLS 127.0.0.1
 ------------------------  -----------------------------------------    ------------------------  ------------
 status|shell|screencap|   policy: Android, feature_enabled, enabled,   key, pairing, TLS,         uid 2000
 input, selftest --gate    unshared (share = none), READY               provenance, streams
                           caller pid -> pane -> session (audit)
                           audit: state/adb/audit.log + Worker log
```

## Go packages

| Package / file | What |
|---|---|
| `internal/worker/devctl` | bridge client, policy, session mapping, audit, input validation, token line |
| `internal/worker/devctl/devctltest` | fake bridge on a Unix socket for tests (never a device) |
| `internal/ops/adb.go` | `adb_status`, `adb_shell`, `adb_screencap`, `adb_input`, `adb_disable` (Unreleased) |
| `internal/worker/agent/exec_adb.go`, `mod_devctl.go` | the ops on the executor; module started on Android only |
| `internal/cli/adb.go`, `adb_selftest.go` | `minion adb status|shell|screencap|input`, `selftest --gate` |
| `internal/hub/web/page_worker_device.go` | Worker page panel: status, last 20 audit entries, Disable |
| `internal/hub/server/devctl_guard.go` | `grant_set` refuses a grant to a Worker with device control on |

## Policy (checked on every call)

1. `GOOS == android`, else `unsupported_platform` (the ops are not even registered elsewhere: `unsupported_op`).
2. The bridge socket answers, else `bridge_unavailable`.
3. The app reports `feature_enabled` (build flag), else `not_available`.
4. The owner enabled it on the device (`enabled`), else `disabled`.
5. The Worker is unshared (`share = none`; `later`, `select` and `all` are shared). A shared Worker with device
   control on is **disabled** at once (fail closed), and the call is refused with `shared`.
6. Bridge state `READY` or `PAIRED` (the bridge connects on demand and checks provenance first), else
   `not_paired` (UNPAIRED), `needs_wifi` (WAITING_WIFI) or `endpoint_unverified`.

`adb_shell`, `adb_screencap` and `adb_input` are refused from the Hub (only a session of this Worker, through
its control socket, can use them). `adb_status` and `adb_disable` are routed by the Hub too; the Hub can
disable, never enable. `share_set` (local or from the Hub) and `worker config share` refuse any mode but
`none` while device control is on; any committed share change disables it; a 5 s watch disables it when the
Worker became shared by a path that did not ask (for example while the Worker was stopped).

## Bridge protocol (Worker -> app), version 1

Socket: the path in `MINION_ADB_BRIDGE_SOCKET` (set by the app's `NativeWorker.startWorker`; it must be an
absolute path ending in `adb-bridge.sock`), else `<runtime dir>/adb-bridge.sock` next to `worker.sock`
(`r/minion/` in the app); a filesystem Unix socket, 0600, peer uid == app uid, served by the app's
`BridgeServer`. No TCP listener anywhere.

Token: the app writes exactly one line to the Worker's stdin at launch, `MINION_ADB_BRIDGE_TOKEN=<64 hex>\n`
(`BridgeToken.stdinLine`). The Worker reads that one line on Android only and sends the token in every request.
It prevents accidental use by other same-uid code; it is not a security boundary.

Frame: `u32 big-endian n` (1 .. 1 MiB + 1), `u8 channel`, `n - 1` payload bytes. Channel 0 is one UTF-8 JSON
object; channel 1 is raw stdout bytes; channel 2 is raw stderr bytes. Large outputs (a screenshot) are split
into frames of at most 1 MiB.

One operation per connection. The Worker sends one channel-0 request (at most 256 KiB):

```
{"version":1,"token":"<hex>","op":"status"}
{"version":1,"token":"<hex>","op":"disable","reason":"local|hub|share"}
{"version":1,"token":"<hex>","op":"shell","command":"sh -c 'exit 7'","timeout_ms":120000}
{"version":1,"token":"<hex>","op":"screencap","timeout_ms":60000}
{"version":1,"token":"<hex>","op":"input","command":"input keyevent KEYCODE_HOME","timeout_ms":30000}
```

- `command` is built and quoted by the Worker: shell with one argument takes it as the command line (like
  `adb shell "ls | wc -l"`), several arguments are POSIX-quoted one by one; input is validated
  (`keyevent` KEYCODE_NAME or number, 1 to 8; `text` printable ASCII up to 1024 bytes, spaces as `%s`;
  `tap X Y`; `swipe X1 Y1 X2 Y2 [MS]`; coordinates 0..10000) and refused locally otherwise (exit 2).
- `timeout_ms` is clamped by both sides to 1 s .. 3600 s (default 120 s).

Answers (channel 0 unless noted):

| Request | Answer |
|---|---|
| status | `{"type":"status","feature_enabled":B,"enabled":B,"state":"UNAVAILABLE|OFF|UNPAIRED|PAIRED|READY|WAITING_WIFI|ENDPOINT_UNVERIFIED","port":N,"key_storage":"android-keystore (TEE|STRONGBOX|SOFTWARE)|app-file","active_streams":N,"last_ok":"RFC 3339","last_error":"<code>"}` |
| disable | `{"type":"ok"}` after every stream is closed |
| shell, screencap, input | `{"type":"started"}`, then any number of channel 1 / 2 frames, then `{"type":"exit","code":N}` |
| any | instead: `{"type":"error","code":"<code>","message":"<short class text>"}` |

Error codes: `not_available`, `disabled`, `not_paired`, `needs_wifi`, `endpoint_unverified`, `timeout`,
`bad_token`, `bad_request`, `busy`, `internal`. The Worker adds `shared`, `bridge_unavailable` and
`unsupported_platform` on its side. Messages are short class texts; they never hold a command, output, a
code, a key or the token.

Cancellation: the Worker closing the connection ends the operation; the bridge closes the adb stream. A
disable makes the bridge end every open stream with `{"type":"error","code":"disabled"}` (the caller exits
255 "device control disabled") and refuse new ones. The Worker dials with a 5 s timeout and expects the first
answer of status and disable within 5 s ("no hang").

## Worker op wire (local control socket)

`adb_shell`, `adb_screencap` and `adb_input` stream the data as `op.progress` elements
`{"stream":"stdout|stderr","data":"<base64>"}` and end with `{"exit_code":N}`. A Minion-side failure is an
error frame whose message is the honest reason ("device control is disabled; ...", "needs Wi-Fi: Wireless
debugging is off ...", "not paired: ... pair again in the Minion app", "endpoint unverified: ...",
"Worker is shared: ...", "timeout"). `adb_status` answers `{"device_control":{...},"audit":[...]}`.

`worker status --json` has a `device_control` block on an Android Worker: `available`, `enabled`, `state`,
`port`, `key_storage`, `active_streams`, `last_ok`, `last_error`, `shared`, `reason`.

## Command line

```
"$MINION_BINARY" adb status [--json]
"$MINION_BINARY" adb shell [--timeout S] -- CMD...      # remote exit code
"$MINION_BINARY" adb screencap OUT.png                  # relative path inside the current directory
"$MINION_BINARY" adb input keyevent|text|tap|swipe ARGS...
"$MINION_BINARY" adb selftest --gate [--report FILE]    # writes ~/dc-gate-report.txt
```

Exit codes: shell and input return the remote exit code; refused arguments exit 2 before anything is sent;
every Minion-side failure exits 255 with the real cause on stderr. `selftest --gate` runs G0 (baseline values),
G3 (`id -u` = 2000), G3b (exit 7, `--timeout 3 -- sleep 10`, 20 fresh calls), G3c (screencap PNG, invalid input
refused locally) and G12 (key storage), and lists G1, G2, G3d and G4 to G11 as MANUAL steps; it exits 1 when an
automatic test fails.

## Audit

One entry per operation in `state/adb/audit.log` (JSON lines, 0600, rotated at 1 MB, 3 old files kept), also
logged by the Worker as `event=device_control.audit` (which carries it to the remote log sinks and OTEL) and
shown on the Hub panel (last 20, through `adb_status`; the Hub's own status reads are not audited, so they do
not push the operations out of that list). Fields, and only these: `time`, `actor` (`local`),
`session` (the tmux session of the caller: the Worker reads the peer pid of the control socket connection from
the kernel and walks `/proc/<pid>/stat` parents up to a pane pid; `unknown` otherwise), `op`
(`status|shell|screencap|input|disable`), `transport` (`tls-loopback`), `result`
(`ok|exit:<n>|denied:<reason>|error:<code>`, `denied:provenance` for an unverified endpoint), `duration_ms`.
The entry type has no field for a command, arguments, output, a screenshot, a pairing code, key material or
the token.

## Hub

- Worker page: a "Device control" panel for an Android Worker (owner or administrator): status, last 20 audit
  entries newest first, and "Disable device control" while it is on. No enable control exists.
- `grant_set` asks the target Worker (`adb_status`, as the caller) before a grant lets sessions of another
  Worker reach it: enabled -> refused (`conflict`, with the reason); not Android or older (`unsupported_op`) and
  offline -> allowed; any other failure -> refused (fail closed). Known limit: a grant to an offline Worker is
  not checked; the Worker cannot see Hub grants.

## What needs the device

Everything that talks to a real adbd or the real app bridge: pairing, TLS with the Keystore key, provenance,
Wi-Fi loss, the stdin token line from `NativeWorker.start()`, peer pid mapping inside the app sandbox, and the
gate G0 to G12. The Go side is tested with the fake bridge only.
