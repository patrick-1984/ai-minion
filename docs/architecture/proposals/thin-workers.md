# Thin Workers for OpenWrt and ESP32 (preview)

Status: proposal and preview implementation, branch `feat/thin-clients`. Not part of a 0.8.x release.

## 1. Goal

A Minion Worker that fits where the 32 MB Go binary cannot:

- **OpenWrt routers**, including 16-32 MB flash MIPS devices (`mips_24kc`, `mipsel_24kc`), ARMv7, aarch64 and
  x86_64, on OpenWrt 24.10 (ipk) and 25.12 (apk).
- **ESP32-family microcontrollers** (ESP32, ESP32-S3, ESP32-C3, ESP32-C6) with 4 MB flash and about 300 KB RAM.

Targets: OpenWrt package under 150 KB installed (TLS from the system `libmbedtls`), ESP32 application under
1.2 MB (fits the default 4 MB layout with two OTA slots), steady-state RAM under 64 KB for the Minion part on
ESP32.

Non-goals for the preview: pairing codes (CPace), the pipe (agent messages), session-control calls (`ca`
streams), agent detection, tmux.

## 2. Decision: speak the existing Worker protocol, not a new one

The Worker protocol (3.0) already has the properties a device needs:

- Trust does not come from TLS. The Worker pins the Hub **root** Ed25519 key and verifies the Hub's operational
  certificate chain inside the handshake (`internal/transport/ws.go` dials with verification off on purpose).
- The Worker authenticates with a long-lived Ed25519 key. Nothing on the Worker side expires.
- Enrollment with an invitation key needs only Ed25519, X25519, SHA-256, HKDF-SHA256 and ChaCha20-Poly1305.
  The only exotic primitive (ristretto255, for CPace pairing codes) is not needed.

So the thin Worker is a second implementation of the device half of the protocol, in C, and the Hub needs **no
change**. A new "lite" protocol was rejected: it needs Hub code, a new security analysis and a second trust model,
and it does not shrink the device by much (the cost is TLS and Wi-Fi, not the handshake).

Rejected alternatives:

| Option | Why not |
|---|---|
| Go Worker trimmed (no Hub, no SQLite) | still 8-15 MB; does not build for 32-bit MIPS with the current SQLite driver; no ESP32 |
| TinyGo | no proven TLS/WebSocket stack on ESP32 or linux/mips |
| New lite protocol + Hub adapter | Hub change, new crypto design, a second trust path to review |
| REST API client with an API key | not a Worker (cannot host sessions); API keys expire (90 days by default) |

## 3. Connect methodology: enroll once, connect forever

The owner rule is: no expiring certificates on devices, nothing to redo after a day, a month or a year.

**What the device stores** (about 150 bytes): Ed25519 seed (32), Hub root public key (32), chain position
`pin_seq` (8), certificate ratchet `max_serial` (8) and `cert_sha256` (32), `worker_id`, the Hub URL.

**Enrollment, once:** the owner creates an invitation key in the Hub (one-time, short TTL is fine) and gives it to
the device (UCI option on OpenWrt, `enroll` command on the ESP32 serial console). The device connects with
`mode=enroll_invite`, checks that the Hub root matches the key's fingerprint, completes the handshake, sends the
secret inside the encrypted channel, and on `welcome` stores the pin and the ratchet. It then **erases the
invitation key**. The Worker is pending until the owner approves it (Hub UI or `minion hub worker approve`).

**Every connection after that** (`mode=connect`):

1. TCP, optional TLS **without certificate verification** (no CA bundle, no leaf pin, nothing that expires; a
   45-day Let's Encrypt certificate on the proxy changes nothing), WebSocket `minion.v1`.
2. Handshake: the device checks that the Hub root equals its pin (or follows a signed root transition chain and
   re-pins), that the operational certificate is signed by that root, inside its window, with a serial `>=` the
   stored maximum, then verifies the Hub signature and signs the transcript with its own key.
3. The ratchet is raised and persisted before any operation is served.

**What can change on the Hub, and what the device does:**

| Hub event | Device action |
|---|---|
| Operational certificate re-issued (new key, new serial) | nothing; next reconnect accepts it (serial is higher) |
| Certificate validity raised to 10 years (Hub PR #24) | nothing; the device accepts up to 3650 days |
| Master (root) rotation | follows the signed transition chain in the Hub hello, re-pins, no user action |
| Proxy / TLS certificate change, IP change | nothing (TLS is not trusted, the URL is a name) |
| Hub down for days, device offline for a year | reconnects with backoff 1 s .. 60 s, forever |
| Worker removed or revoked in the Hub | only a **signed** `revoke` wipes the enrollment; then the device idles |
| Certificate expired and not re-issued | refuses (protocol rule), keeps its enrollment, retries; Hub-side problem |

**Clock.** The certificate window check needs wall time. OpenWrt has `sysntpd`. The ESP32 runs SNTP (pool or the
DHCP-provided server) and does not connect before time is set (year >= 2025). This is the only external
dependency, and it is not something the owner redoes.

**Not used on devices:** pairing codes. The invitation key is the "connect once" method: it does not require a
person at the device within a 10-minute window, and it is easy to deliver over a serial console or UCI.

## 4. Architecture

```
clients/
  core/            portable C99, no OS calls; unit-tested on the host
    sha256.c hmac/hkdf      own (small, RFC vectors)
    third_party/monocypher  X25519, Ed25519 (SHA-512 variant), ChaCha20-Poly1305 (IETF)
    b64.c b32.c json.c      strict base64url, base32 (fingerprints, invite keys), jsmn-style parser + emitter
    ws.c                    RFC 6455 client: upgrade, masking, binary, ping/pong/close, reassembly
    hs.c cert.c chain.c     handshake (connect + enroll_invite), 153-byte cert, ratchet, root transitions
    chan.c                  record layer: 4-byte length + ChaCha20-Poly1305, 64-bit counters
    yamux.c                 client role: SYN/ACK/FIN/RST, window updates, ping, go-away
    worker.c                frames, welcome/revoke, op server, ev stream (snapshot + events)
    vt.c                    small VT100/ANSI screen model (grid + scrollback, SGR to spans, hash)
    session.c keys.c        session table, tmux key names to bytes, write cache (request ids)
    mt.h                    platform hooks: net, time, random, store, log, session I/O
  host/            Linux build (plain sockets; TLS via mbedTLS when available); interop tests vs a real Hub
  openwrt/         package Makefile (feed layout), procd init, UCI config, PTY sessions (forkpty /bin/ash)
  esp32/           ESP-IDF project: Wi-Fi, SNTP, NVS store, esp-tls/mbedTLS, sessions below
```

One event loop per platform (poll on OpenWrt and host, a FreeRTOS task with select on ESP32). The core is
single-threaded and calls back into the platform; there is no dynamic library dependency except `libmbedtls` on
OpenWrt (already in default images) and ESP-IDF components on ESP32.

**Operations served:** `sessions_list`, `session_snapshot`, `session_capture`, `send_text`, `send_keys`,
`share_get`, `share_set` (and the internal `share.probe`), plus the `ev` stream that fills the Hub's live session
list. Not served: `session_create`, `set_task`, `answer`, pipe operations. The Hub only routes operations the
Worker announces in its hello.

**Write semantics:** UUIDv7 request ids, a fixed ring of recent results (32 on ESP32, 256 on OpenWrt) answers
duplicates for 10 minutes; a duplicate older than the ring is refused rather than re-executed. `origin` is
required on `send_text`/`send_keys` as in the Go Worker. `preview_only` from the Hub policy refuses writes.

## 5. Sessions per platform

**OpenWrt:** each configured session is a PTY running a command (default `shell` = `/bin/ash -l`), restarted on
exit, rendered by `vt.c` (80x24 default, scrollback 200 lines). Optional sessions: `logread -f` (read-only),
`dmesg -w`. No tmux needed.

**ESP32** has no shell, so the thin Worker brings its own sessions:

| Session | What it is | Input |
|---|---|---|
| `console` | built-in command shell (below) | commands via `send_text` + Enter |
| `uart` | serial bridge to an attached device (pins, baud configurable): a router's or SBC's serial console becomes a Minion session | raw bytes, keys (C-c, arrows) |
| `log` | ESP-IDF log output (read-only) | none |

Console commands (each prints a short result, errors with `ERR ...`):

```
help                         status (chip, IDF, uptime, reset reason, heap, Wi-Fi, IP, RSSI, time)
gpio list                    allowed pins with mode and level
gpio mode <pin> in|out|in_pullup|in_pulldown|od
gpio read <pin> | gpio write <pin> 0|1 | gpio toggle <pin> | gpio pulse <pin> <ms>
gpio watch <pin> on|off      print level changes (debounced) to console
adc read <pin>               millivolts (calibrated where supported)
pwm set <pin> <freq_hz> <duty_pct> | pwm off <pin>
i2c scan [sda scl]           7-bit addresses found
wifi status | wifi scan | wifi set <ssid> <password> (stored, reconnects)
uart set <baud> [tx rx]      settings of the uart session
time                         wall time and SNTP state
config get|set <key> [value] NVS settings (hub url, name, pins allowlist)
enroll <mnk_...>             enroll once (also available over USB serial before Wi-Fi works)
reboot
```

Safety: GPIO commands act only on pins in an allowlist (default: none; set with `config set gpio_allow`), and the
flash/PSRAM and strapping pins are refused for each chip. Writes from the Hub need the `operate` action and are
refused under `preview_only`.

## 6. Build and release tool chain

- `clients/host`: `make -C clients/host test` builds the core with `-Wall -Wextra -Werror`, runs unit tests (RFC
  vectors, the repository's golden vectors `internal/identity/testdata/hub_cert_vectors.json` and
  `internal/transport/testdata/handshake_vectors.json`), then an **interop test**: builds the Go Hub from this
  repository, enrolls the thin Worker with an invitation key, approves it, and drives `sessions_list`,
  `session_snapshot`, `send_text`, `send_keys` through the Hub REST API.
- OpenWrt: `openwrt/gh-action-sdk` matrix over 24.10 (ipk) and 25.12 (apk) SDKs for `mips_24kc`,
  `mipsel_24kc`, `arm_cortex-a7_neon-vfpv4`, `aarch64_cortex-a53`, `x86_64`.
- ESP32: `espressif/esp-idf-ci-action` (ESP-IDF v5.4) for `esp32`, `esp32s3`, `esp32c3`, `esp32c6`; one merged
  image per chip for flashing at offset 0 (`esptool.py write_flash 0x0 ...`).
- Workflow `.github/workflows/thin-workers.yml`: tests on pull requests touching `clients/**`; on a tag
  `thin-v*` it builds everything and creates a **GitHub prerelease** with the packages, images and
  `SHA256SUMS`. It never touches the `v*` release flow, the ghcr channels or "latest".

## 7. Risks

- Second implementation of security-critical code (handshake, certificate checks, record layer). Mitigation:
  golden vectors from the Go implementation, the interop test against the real Hub on every change, Codex
  adversarial review, and a narrow surface (no pairing codes).
- Monocypher's Ed25519 variant must match RFC 8032 (SHA-512); verified by RFC 8032 test vectors.
- ESP32 heap: TLS (~40 KB) plus a 16 KB record buffer; keep one connection, reuse buffers, no PSRAM required.
- yamux window rules: the device processes stream data as it arrives and returns window credit at once; frames
  larger than the device limit (16 KB on ESP32) reset that stream instead of buffering.
