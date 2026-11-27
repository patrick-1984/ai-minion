# Installing Android apps

Back to the [README](../../README.md).

From v0.25.0 the Minion app on an Android Worker (a phone, a tablet or a TV
box) installs other apps for you, without adb. You copy the APK into the
app's **Minion files** folder with [file transfer](file-transfer.md), then ask
the Worker to install it. The app checks the APK and installs it with
Android's PackageInstaller.

## The one-time step

Minion needs **Install unknown apps** allowed on the device. This is the same
switch the [self-update](android-updates.md#the-one-time-step) uses: if Minion
already updates itself, nothing is left to do. Otherwise open Minion, choose
**Allow Minion to install updates?** and turn the switch on (or, from a PC,
`adb shell appops set cc.nasz.minion REQUEST_INSTALL_PACKAGES allow`).

`minion apps status` shows `ready: no` with the reason while the switch is
off.

## How to install an app

```sh
# 1. Copy the APK into Minion files. On Android every transfer of a session
#    uses the Minion files folder: a session outside it receives in
#    "Minion files" itself.
minion files push --target <android-worker-id>:<session> ./example.apk

# 2. Install it. --allow-new is needed for an app that is not installed yet.
minion apps install --target <android-worker> example.apk --allow-new

# Later: an update of the same app (same signer, higher version) needs no flag.
minion files push --target <android-worker-id>:<session> ./example-1.1.apk --overwrite
minion apps install --target <android-worker> example-1.1.apk

# What the Worker can install, the last attempt and the APK files it sees.
minion apps status --target <android-worker>
```

- `--target` is the Worker's id or its name. The path is relative to the
  Minion files folder (`apps/example.apk` for a file in a sub folder).
- The Hub authorizes and audits every install. With `MINION_HUB_URL` and
  `MINION_API_KEY_FILE` the commands call the Hub with that key. Without them,
  in a session of another Worker, the local Worker sends the request to its
  Hub: it is allowed for a session whose grant or master acceptance has the
  box **Install apps** (`apps_install`, see [Who may install](#who-may-install)).
  A session on the Android device itself is refused (see
  [Security trade-offs](#security-trade-offs)).
- Exit status of `install`: 0 for `installed`, `needs_confirmation_on_device`
  and `installing`; 3 for `refused`; 1 for `failed` or an error; 2 for a usage
  error. `--json` prints the result.
- **MCP**: `minion mcp` offers `app_install` as a write tool (only with writes
  allowed, and subject to the standing allow rules of
  [`minion claude allow`](claude-code-standing-allow.md)), and
  `app_install_status` as a read tool.
- **Web**: on the page of an Android Worker, owners and administrators find
  **Install an app from Minion files**: it lists the `.apk` files at the top of
  the folder with an **Install** button and shows the outcome.

## The first install needs one press on the device

Android installs an app silently only when the installing app is the
installer of record of that package. For a first install Android asks once
on the device:

- The Minion screen shows an **Install <app>** button. On a TV it takes one
  press of the D-pad's center button (no touch needed); Android's own
  confirmation dialog follows.
- Minion also shows a notification that opens the Minion screen.
- The outcome of the call is `needs_confirmation_on_device`. After the press,
  `minion apps status` shows `installed`.

From then on Minion is the installer of record: **updates of an app Minion
installed run without a dialog** on Android 12 and newer. Android 10 and 11
always ask. An app installed by another store or by adb asks on its first
update through Minion.

## Checks before anything is installed

The Worker:

1. confines the path to the Minion files folder: relative, no `..`, no
   symbolic link on the way, a regular file named `*.apk`, at most 512 MiB;
2. computes the file's SHA-256 and hands the request (path, digest, size,
   `allow_new`) to the app in its private storage.

The app:

1. copies the APK, from one open of the file, into its private storage and
   requires the copy to have the SHA-256 and size of the request (a file
   changed after the request is refused); every later step reads this copy
   only, so the checks, the audit and the install see the same bytes;
2. reads the copy with Android's package manager: package name, version
   code, version name, label and signing certificate;
3. refuses the Minion app itself (Minion updates itself with its own
   [update path](android-updates.md));
4. for an installed app: requires the same signing certificate as the
   installed one and a version code at least as high (no downgrade); the same
   version is reinstalled only with `--allow-new`;
5. for an app that is not installed: requires `--allow-new`;
6. streams the copy into the install session and removes it;
7. reads the request once more right before the commit; a withdrawn request
   installs nothing.

Android itself then refuses an APK with another signer or a lower version
than the installed app.

## Outcomes

| Outcome | Meaning |
|---|---|
| `installed` | Android installed the app |
| `needs_confirmation_on_device` | Android asks on the device: press **Install <app>** on the Minion screen |
| `installing` | the app is still checking or installing when the call returns; see `minion apps status` |
| `refused` | a check refused the APK, or the install was cancelled on the device; the reason says which |
| `failed` | Android or the handover failed; the reason says why |

The result also names the package, the version code and name, the signer's
SHA-256, the APK's SHA-256 and size, and whether it is a new install.

## Who may install

| Caller | Authority |
|---|---|
| Administrator | every Android Worker |
| Owner | own Android Workers |
| API key | only when its scope names the action `app_install`; a key with "everything", a wildcard or the file actions has none |
| Session on another Worker | only through a Control grant or master acceptance with the box **Install apps on** the device (`apps_install`, default off): `minion apps install` (or the MCP tool) in the session, without a Hub API key, goes there through the local Worker. **Send files to target** never implies it. A session without such a grant or acceptance is refused with `forbidden` |
| Worker identity | none |
| A session on the Android device itself (local socket) | none: the Worker refuses `app_install` that does not come through the Hub |

Tick **Install apps on** the device when you add the grant on **Access >
Grants > Add grant** (it is offered once the chosen targets are on an Android
Worker, and needs Control), or later with **Edit** on the grant's row. The
grant is saved and usable in that one step. For nominated master sessions,
administrators tick the same box under the accepted Worker on **Access >
Minion control**, on a whole-Worker or a limited acceptance (the session list
stays as it is).

From a session on Worker A, with a Control grant to the Android Worker that
has **Install apps** ticked (and **Send files to target** for the push):

```sh
minion files push --target <android-worker-id>:<session> ./example.apk
minion apps install --target <android-worker> example.apk --allow-new
```

The action `app_install` implies nothing and nothing implies it: `operate`
does not allow an install, and neither does `file_write`.

**Audit.** The Hub writes every request to its audit log (action
`app_install`): the Worker, who asked, the package, the version code, the
signer's SHA-256 and the outcome. File contents are never logged.

## Security trade-offs

- **Installing an app is powerful.** An installed app runs with its own
  permissions and can start at boot. Give `apps_install` only to sessions you
  would let install software on the device, and API keys the action
  `app_install` only when needed.
- **Updates are silent once Minion installed an app.** Whoever may run
  `app_install` on the Worker can replace such an app with any newer version
  signed with the same key. The signer check keeps a different key out.
- **Sessions on the device itself.** Every terminal session of the Android
  Worker runs as the Minion app's user and can write the app's private files,
  including the install request. Treat a session on the device as able to
  request installs: a first install still needs the press on the device, but
  an update of an app Minion installed does not. Do not run untrusted agents
  in sessions of an Android Worker that installs apps.
- **The switch on the device wins.** Turning **Install unknown apps** off for
  Minion stops every install (and self-update) at once; the Hub cannot turn
  it back on.
- **Play Protect** may scan an APK or block an install. This is not tested on
  every device.
- **Uninstalling is manual**: Minion does not remove apps. Use the device's
  settings.

See also [File transfer](file-transfer.md), [Android app updates](android-updates.md),
[Access: who reaches what](access.md) and the [Android README](../../android/README.md).

Back to the [README](../../README.md).
