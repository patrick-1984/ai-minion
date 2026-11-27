# Device control (on-device adb): Android side and bridge protocol

Design: `docs/minion-android-adb/DESIGN.md` v2 (sections 2-5, 9, 10) with the K0 desk corrections.
This file is the contract between the app (Kotlin, `cc.nasz.minion.devicecontrol`) and the Go Worker (WP G1).

## Build flag (compiled in, off)

| Build | Device control |
|---|---|
| any build without `-PminionDeviceControl=true` (all normal releases) | `BuildConfig.DEVICE_CONTROL = false`. No provider, no screen, no `SYSTEM_ALERT_WINDOW`, no `libminion_spake2.so`, no bridge socket, no token, no `MINION_ADB_BRIDGE_SOCKET`. The Kotlin classes are compiled in but unreachable (no manifest entry, the only UI button is behind the constant). |
| `-PminionDeviceControl=true` (gate RC, release or debug) | Overlay manifest `app/src/deviceControl/AndroidManifest.xml` is merged; the SPAKE2 JNI shim is built for `arm64-v8a` and `armeabi-v7a`. Still off until the owner enables it on the device. Needs API 31+. |

```
./gradlew :app:assembleRelease -PminionDeviceControl=true [-PminionBoringsslDir=<checkout of 0.20261020.0>]
./gradlew :app:testDebugUnitTest -PminionSpake2HostLib=<host build>/libminion_spake2.so   # host build: see app/src/main/cpp/CMakeLists.txt
```

Flipping the default is a one-line change after the Nebula gate (design 9).

## Transports (legacy adb first, then TLS)

The bridge keeps one verified adb connection and tries, in this order:

1. **Legacy adb on `127.0.0.1:5555`** (`AdbLegacy.kt`, transport `legacy-loopback`). Used where the firmware
   already runs adbd on TCP 5555 (`service.adb.tcp.port=5555`, as on the owner's projector); Minion never opens,
   enables or changes that port. Works without Wi-Fi and without pairing. Handshake (AOSP `protocol.txt`,
   `client/auth.cpp`, `daemon/auth.cpp`): `CNXN` -> adbd `AUTH TOKEN` (20 random bytes) -> `AUTH SIGNATURE` =
   RSA PKCS#1 v1.5 over the SHA-1 DigestInfo of the raw token (`RSA_sign(NID_sha1, token, 20)`; the token is not
   hashed again), made with the same adb key as the TLS path (`NONEwithRSA` over the 35-byte DigestInfo; raw
   `RSA/ECB/NoPadding` over the EMSA block as fallback). If adbd knows the key it answers `CNXN` (silent). If it
   sends a second `TOKEN`, the app sends `AUTH RSAPUBLICKEY` (`base64(android RSA pubkey struct) + " minion@cc.nasz.minion" + NUL`)
   and the TV shows **"Allow debugging?"**: state `AWAITING_AUTHORIZATION`, calls get `needs_authorization`,
   the app waits up to 120 s for the owner's Allow (tick "Always allow from this computer" so later connections
   are silent). No answer or Deny: `needs_authorization`; the next call asks again (never on a status poll).
   A port that answers `STLS` is not legacy and is left to the TLS path. Cleartext adb on loopback does not leave
   the device; the LAN exposure of 5555 is the firmware's and is reported by the gate (G1/G7), not used by Minion.
2. **Wireless debugging (TLS)** on the connection port (NSD or manual), transport `tls-loopback`, as below. Needs
   Wi-Fi and a pairing. Used only when nothing speaks legacy adb on 5555.

Both transports dial only `127.0.0.1`, never another address, and both run the provenance check (below) on every
new connection before any operation. A legacy endpoint that fails provenance is `endpoint_unverified` and the
bridge then tries TLS; if TLS is not there either, the legacy failure is reported.

## Environment the app sets

| Variable | Who gets it | Value |
|---|---|---|
| `MINION_BINARY` | tmux server, Worker, panes (every start) | `<nativeLibraryDir>/libminion.so` (changes on APK update) |
| `ENV` | same | `r/minion.shrc`: `alias minion='"$MINION_BINARY"'`, then sources `~/.mkshrc` if present |
| `MINION_ADB_BRIDGE_SOCKET` | Worker only, flag builds only | `<noBackupFilesDir>/r/minion/adb-bridge.sock` |

Token: in flag builds the app writes exactly one line to the Worker's stdin at launch and closes stdin:
`MINION_ADB_BRIDGE_TOKEN=<64 lowercase hex>\n`. A new token and a new socket for every Worker start. Read it only
when `MINION_ADB_BRIDGE_SOCKET` is set. Never log it, never put it in an event. It is not a security boundary
(same uid, design 5); it stops accidental use.

## Socket

Filesystem Unix stream socket, mode 0600 in a 0700 directory; the app accepts only peers with its own uid.
One operation per connection; the client opens a new connection per call. At most 8 operations at once
(`busy` otherwise).

Frame: `u32 big-endian n` (1 .. 1 MiB + 1), `u8 channel`, `n-1` payload bytes.
Channel 0 = one JSON object (UTF-8), 1 = stdout bytes, 2 = stderr bytes.

### Request (client -> app, first frame, channel 0, within 5 s)

```
{"version":1,"token":"<hex>","op":"status"}
{"version":1,"token":"<hex>","op":"disable","reason":"app|hub|share|local"}
{"version":1,"token":"<hex>","op":"shell","command":"sh -c 'exit 7'","timeout_ms":120000}
{"version":1,"token":"<hex>","op":"screencap","timeout_ms":60000}
{"version":1,"token":"<hex>","op":"input","command":"input keyevent KEYCODE_HOME"}
```

- `shell.command`: the command line the Worker built (`devctl.ShellCommand`: one argument as typed, several each
  quoted). `argv` (a JSON array, same rule) is accepted instead. Runs `shell,v2,raw:` (no PTY), stdin closed.
  `timeout_ms` default 120000, capped at 3600000.
- `input.command`: the Worker's `devctl.InputCommand` line. The app splits it back (bare safe words and `'...'`
  quoting only; anything else is `bad_request`) and accepts only `keyevent KEYCODE_X|N...` (1-8),
  `text <one arg, no control chars>`, `tap X Y`, `swipe X1 Y1 X2 Y2 [MS]` (digits), then re-quotes it.
  `args` (a JSON array without `input`) is accepted instead.
- `screencap`: `exec:screencap -p`, raw PNG on channel 1, `timeout_ms` default 60000.

### Responses (app -> client)

```
status:   {"type":"status","feature_enabled":true,"enabled":true,"state":"READY","port":37123,"port_source":"nsd|manual|legacy",
           "transport":"legacy-loopback|tls-loopback|",
           "key_storage":"android-keystore (TEE)|app-file|","key_warning":"","active_streams":0,
           "last_ok":"<RFC 3339>","last_error":"<error code>","last_error_message":"<text>","provenance_denied":0}
disable:  {"type":"ok"}
ops:      {"type":"started"}  then channel 1/2 frames  then {"type":"exit","code":<remote exit code>}
error:    {"type":"error","code":"<code>","message":"<text for the user>"}   (at any point, then the socket closes)
```

`state`: `UNAVAILABLE`, `OFF`, `UNPAIRED` (no legacy adb and not paired), `PAIRED` (connects on demand: paired,
or legacy adb seen), `AWAITING_AUTHORIZATION` (legacy: "Allow debugging?" is on the TV), `WAITING_WIFI`,
`ENDPOINT_UNVERIFIED`, `READY`. `transport` is set while connected (`legacy-loopback` also while the TV asks);
`port` is 5555 for legacy.

Error codes (the CLI maps all of them to exit 255 with the message): `bad_token`, `bad_request`, `busy`,
`not_available`, `disabled`, `shared`, `not_paired`, `needs_authorization` (legacy: confirm the TV dialog),
`needs_wifi`, `ambiguous` (several local adb endpoints;
the message says to enter the connection port), `endpoint_unverified`, `timeout`, `internal`.
These match `internal/worker/devctl/protocol.go`.

Disable (app button, `op: disable`, or a share change seen in `worker status --json`) closes the one adb
connection, which ends every running stream at once: running calls get `error disabled` (or
`needs_wifi`), new calls get `disabled`.

Cancel: the Worker sends nothing after its request, so the bridge reads its side of the socket during an
operation; end of input (the caller left, its timeout, the Worker's own disable) closes that adb stream at once,
also while the command prints nothing. Other streams and the connection stay up.

WAITING_WIFI / UNPAIRED: the Worker refuses calls in these states without asking the bridge, so a status request
in them starts a background reconnect (at most every 5 s: legacy 5555 first, then a known TLS endpoint); the next
call finds READY. Enabling and every Worker start also try to connect at once, so the TV dialog of a first legacy
connection appears right after Enable. ENDPOINT_UNVERIFIED stays until the endpoint changes (NSD) or the owner sets the connection port.

The app never enables on a request from the socket; there is no `enable` op.

## What the app does on its own

- Enable only on the device (confirm text of design 5) and only while the Worker reports `share.mode = none`.
  While enabled the app's share switch is refused; if the report shows sharing on, the app disables (fail closed).
- Pairing: overlay keypad (`TYPE_APPLICATION_OVERLAY`, `FLAG_SECURE`, 3 min), code in a `CharArray` only, wiped
  after use, never sent to the Worker, never logged. `_adb-tls-pairing._tcp` NSD hint prefills the port.
- Connection: legacy adb on `127.0.0.1:5555` first (above); then `_adb-tls-connect._tcp` NSD records filtered to this device's own interface addresses, or the
  manual connection port (wins when set); several local candidates = `ambiguous`, never auto-dialled. Dials only
  `127.0.0.1`. No port scan, no `tcpip:`, no setting writes.
- Provenance (design 4.4) on every new connection: `content call --uri content://cc.nasz.minion.adbprobe
  --method prove --arg <128-bit nonce>`; `ProvenanceProvider` accepts only `Binder.getCallingUid() == 2000` with
  the pending nonce; READY only if seen within 5 s and the shell exited 0.
- Key: AndroidKeyStore RSA-2048, purpose SIGN, digests SHA-256 + NONE, signature paddings PSS + PKCS1, encryption
  padding NONE (K0 Q2); fallback `state/adb/adbkey` (0600) with the warning text in `key_warning`.
- Audit: app-side events (`enable`, `disable`, `pair`, `authorize` (legacy TV dialog: `pending`, `ok`,
  `denied:not_allowed`), `provenance`, `revoked`) with their `transport` are appended as JSON lines to
  `state/adb/audit.log` (0600) with `actor=local`, `session=app`; no codes, keys, commands or output. The Worker
  (G3) owns rotation and mirroring.

## Needs the device (gate, design 9)

Legacy adb on 127.0.0.1:5555 with the real adbd (token signature accepted by `RSA_verify`, the "Allow debugging?"
dialog on the TV, silent reconnect after "Always allow", after a cold boot without Wi-Fi), pairing with real adbd
(SPAKE2 + exporter interop), the Keystore key signing the TLS 1.3 handshake, the overlay
over the TvSettings pairing screen without `onPause()`, NSD behaviour of the Nebula, `content call` provenance as
uid 2000, Wi-Fi loss handling, and all G-tests.
