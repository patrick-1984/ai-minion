# Android device control (on-device adb)

Back to the [README](../../README.md).

**Status: present but off.** The code ships in the Android app and the Worker, but every normal build, release
builds included, has device control switched off: there is no Device control button, no extra permission and no
native SPAKE2 library, and `minion adb ...` answers "device control is not available in this build". It is
switched on only in a gate RC (a build made with `-PminionDeviceControl=true`) until the device acceptance tests
pass on the target TV. This guide describes how it works once it is on.

## What it does

Device control lets the sessions of a Minion Worker that runs on an Android device (the Minion app, for example
on an Android TV) run a small set of adb operations on that same device, with the rights of Android's `shell`
user (uid 2000). No PC, no root, no host adb server.

```
 tmux pane                      Worker (libminion.so)        Minion app (Kotlin)              adbd (uid 2000)
 "$MINION_BINARY" adb shell id  policy, session, audit  -->  adb client: key, TLS 1.3,  -->  127.0.0.1:<port>
                                (unix socket, 0600)          provenance check, pairing       (Wireless debugging)
```

From a terminal of the Worker:

| Command | What it does |
| --- | --- |
| `"$MINION_BINARY" adb status` (`--json`) | build flag, enabled, state, connection port, key storage, active streams, last success and last error |
| `"$MINION_BINARY" adb shell [--timeout S] -- CMD...` | runs `CMD` non-interactively (`shell,v2`); prints stdout and stderr, exits with the remote exit code. Default timeout 120 s, at most 3600 s |
| `"$MINION_BINARY" adb screencap OUT.png` | writes a PNG of the screen (`exec:screencap -p`) |
| `"$MINION_BINARY" adb input keyevent\|text\|tap\|swipe ARGS...` | sends key presses, text, taps or swipes; the arguments are checked first and refused with exit 2 if invalid |
| `"$MINION_BINARY" adb selftest --gate` | the automatic parts of the release gate (for the gate run only) |

In an interactive shell of the app, `minion` is an alias for `"$MINION_BINARY"`. A Minion-side failure exits 255
with the real cause, for example "device control is disabled", "needs Wi-Fi: Wireless debugging is off", "not
paired", "endpoint unverified" or "Worker is shared". Nothing hangs: dialling takes at most 5 s.

Hub and MCP: the same operations are the Worker ops `adb_status`, `adb_shell`, `adb_screencap`, `adb_input` and
`adb_disable`. They are marked Unreleased in the op catalog while the feature is off. The Hub Worker page shows a
**Device control** panel (state, port, key storage, last entries of the audit) with a **Disable** button. The Hub
can disable device control, never enable it.

What it never does: it never sends `tcpip:` (no cleartext adb on port 5555), never starts an adb server (nothing
on port 5037), never listens on TCP, never scans ports, never dials anything but `127.0.0.1`, never changes a
setting by itself, never offers install, push/pull, forward/reverse, reboot or root.

## Before you start

- An Android device with API 31 or newer (the target is Android TV 14, API 34) and the Minion app.
- The device on **Wi-Fi**. Android turns Wireless debugging off without Wi-Fi (see Limits).
- A **dedicated, unshared Worker**: its share setting must be `none` and no other Hub user may have terminal
  access to it. Device control refuses to turn on otherwise, and turns itself off if sharing is turned on.

## One-time steps

1. **Developer options.** On the TV: Settings > Device Preferences (or System) > About > press **Build** (Android
   TV OS build) seven times.
2. **Wireless debugging.** Settings > Developer options > **Wireless debugging**: on, and allow it on this network
   ("Always allow on this network"). USB debugging is not needed.
3. **Unshare the Worker.** In the Minion app, turn sharing off (or `minion worker config share none`). The Worker
   must be running, so the app can see the share setting.
4. **Enable.** Minion app > **Device control** > **Enable device control**. Read the confirmation and choose
   **Enable**:
   "Sessions of this Worker get shell-level control of this TV (install apps, change settings, read the screen,
   send key presses) while Wireless debugging is on."
5. **Pair.** **Pair (type the code from Settings)**. If Android asks, allow **Display over other apps** for Minion
   (the pairing keypad is a small window over Settings; it exists only while you pair). Then open Settings >
   Developer options > Wireless debugging > **Pair device with pairing code**. Keep that screen open: the Minion
   keypad appears on top of it with the pairing port filled in. Move to the keypad with the D-pad, type the six
   digits and confirm. "Paired as minion@cc.nasz.minion" means it worked; Settings > Wireless debugging > Paired
   devices now lists `minion@cc.nasz.minion`.
6. Optional: revoke **Display over other apps** again; it is needed only for pairing.

The pairing code stays in the app's memory, shown as dots, and is wiped after use. It never reaches a terminal,
the Worker, the Hub or a log. A wrong or expired code is not retried: open the pairing screen again for a new
code.

If the "Display over other apps" page does not exist on your TV, the one-time fallback is from a PC that is
already paired with the TV: `adb shell appops set cc.nasz.minion SYSTEM_ALERT_WINDOW allow`.

**Connection port.** Normally the app finds the port by itself (it listens for the `_adb-tls-connect` record of
this device only). If it cannot, or if it says several endpoints were found, enter the port shown under Settings >
Wireless debugging > IP address & Port in **Connection port** (empty means automatic). The port changes every time
Wireless debugging is turned off and on.

Check it from a terminal of the Worker:

```
"$MINION_BINARY" adb status
"$MINION_BINARY" adb shell -- id -u        # prints 2000
```

## Status

The app screen and the Minion notification show one line: "Device control: on (TLS loopback, port N)",
"paired, not connected", "waiting for Wi-Fi", "endpoint unverified", "not paired", "off" or "not available in
this build". Android's own Wireless debugging notification stays visible as well. `adb status` and the Hub panel
show the same state:

| State | Meaning |
| --- | --- |
| `UNAVAILABLE` | this build has no device control (the normal case until the gate passes), or Android older than 12 |
| `OFF` | not enabled on the device |
| `UNPAIRED` | enabled, but not paired, or Android ended the authorization: pair again |
| `PAIRED` | paired, no connection open now; the next call connects |
| `READY` | connected over TLS to `127.0.0.1` and the endpoint proved it is this device's adbd |
| `WAITING_WIFI` | no adb endpoint: Wi-Fi or Wireless debugging is off |
| `ENDPOINT_UNVERIFIED` | something answers on the port but did not prove it is adbd; nothing was sent |

## Limits

- **No Wi-Fi, no device control.** Android 14 enables Wireless debugging only while the device is associated with
  a Wi-Fi network, and turns it off when Wi-Fi drops or the access point (BSSID) changes. Ethernet only, the TV as
  a hotspot, or no network at all: not supported. A phone hotspot works when the TV is its Wi-Fi client. During an
  outage every call fails within about 5 s with "needs Wi-Fi"; when Wi-Fi and Wireless debugging are back, calls
  work again without pairing.
- **Authorization lifetime.** Android forgets an adb authorization that was not used for 7 days (its own
  setting). Then the state is `UNPAIRED` and calls say "pair again". Minion never re-pairs by itself.
- **Owner on the device for the first step.** Pairing needs the code shown on the TV.
- Shell uid is not root (`adb root` does not work on user builds). There is no interactive TTY shell.
- If Developer options or Wireless debugging is turned off, or the pairing is forgotten, everything stops and is
  reported. Minion never works around it.

## Security trade-off

Read this before you enable it.

- `shell` is full shell-uid power: `pm` (install and remove apps), `settings`, `cmd`, `am`, reading the screen,
  sending input. "A small set of operations" describes what Minion opens towards adbd, not what a shell command
  can do. This is the power you approve when you enable it.
- **Every terminal of this Worker can use it.** The Worker, the adb client in the app, its key and every tmux pane
  run as the same Android app user. The socket mode, the bridge token and the Keystore key do not stop a pane of
  this Worker. That is why device control works only on a dedicated, unshared Worker, and why sharing is refused
  while it is on (app share switch, `worker config share all`, Hub grants of terminal access to other users). If
  sharing changes anyway, device control turns itself off and ends every running call.
- **Key.** The adb key is an RSA-2048 key in Android Keystore that cannot be exported (`adb status` shows
  `key_storage = android-keystore (TEE|STRONGBOX|SOFTWARE)`). If Keystore cannot be used, the key is a file in
  the app's private storage and the app and Hub say: "Key stored in an app-private file. Every terminal of this
  Worker could read it."
- **Fake adbd.** Another app could listen on a loopback port and pretend to be adbd. After every new connection
  Minion asks the endpoint to call back into the Minion app as uid 2000 with a fresh random value; only the real
  adbd can. Otherwise the state is `ENDPOINT_UNVERIFIED` and nothing is sent.
- **Audit.** Every operation is recorded in `state/adb/audit.log` (JSON lines, 0600, rotated at 1 MB, 3 old files
  kept), in the Worker log (and so in the remote log sinks and OTEL) and on the Hub panel (last 20): time, actor,
  session, operation, result, duration. Commands, arguments, output,
  screenshots, pairing codes, keys and the bridge token are never recorded.
- Enabling is possible only on the device itself. Disabling is possible anywhere.

## How to disable

Any of these ends every running call at once (the caller exits 255 "device control disabled") and refuses new
ones:

- Minion app > Device control > **Disable device control**.
- Hub > the Worker's page > **Device control** > **Disable** (or the op `adb_disable`).
- Turning sharing on for the Worker.

To remove the authorization from Android as well: Settings > Developer options > Wireless debugging > Paired
devices > `minion@cc.nasz.minion` > **Forget** (the app's **How to remove the authorization from Android**
shows the same path). Turn Wireless debugging off when you do not need it. Uninstalling the Minion app removes its
key.

## Licences

Device control builds add `lib/<abi>/libminion_spake2.so`: Minion's Apache-2.0 JNI shim with BoringSSL (release
tag 0.20261020.0, Apache-2.0) linked in for SPAKE2 pairing. Its notice is in the APK at
`assets/licenses/boringssl-LICENSE.txt`. The adb client itself is Minion code (Apache-2.0); no libadb-android,
BouncyCastle or LGPL code is included.
