# Android app updates

From v0.24.0 the Minion app on Android updates itself. You no longer install
each new APK by hand. The Hub sends the update, the Worker inside the app
downloads and verifies it, and the app installs it with Android's
PackageInstaller. On Android 12 and newer this runs without a dialog.

## The one-time step

Do this once, for the first version that contains the updater (v0.24.0 or
newer):

1. Install that APK by hand, as before. From a PC:
   `adb install -r minion-android-<version>.apk`. You can also open the APK on
   the device. The update keeps the pairing and the sessions' state.
2. Open Minion on the device. After the Hub setup it asks **Allow Minion to
   install updates?** Choose it to open the **Install unknown apps** page for
   Minion, and turn the switch on. With a D-pad, that is one switch. On Google
   TV the page can also be under Settings > Privacy (or Apps) > Security &
   restrictions > Unknown sources. From a PC, this command does the same:
   `adb shell appops set cc.nasz.minion REQUEST_INSTALL_PACKAGES allow`.
3. The Minion screen then shows **Install unknown apps: allowed**, and on the
   Hub the Worker gets an **Update** button when it runs an older release than
   the Hub.

The switch stays on across updates. If you turn it off, the Hub shows why the
Worker cannot update itself.

## What happens during an update

```
 Hub: Update (or auto)        Worker in the app                      App (Kotlin)              Android
 ─────────────────────        ─────────────────                      ────────────              ───────
 worker_update vX.Y.Z  ─────► download minion-android-X.Y.Z.apk
                              verify the Sigstore signature of the
                              release and the APK's SHA-256
                              stage it in the app's private storage
                              wait until no client is attached and
                              no agent is busy (unless Update now)
                              hold new sessions, hand over  ───────► run apk-verify again,
                                                                     check package, version
                                                                     code and the pinned
                                                                     signing certificate,
                                                                     stream + hash into a
                                                                     PackageInstaller session ─► install, stop the app
                                                                                                 MY_PACKAGE_REPLACED
                                                                     start the Worker again  ◄──
 Version column: X.Y.Z ◄───── reconnect; kept sessions restored
```

The Workers page shows each step next to the version:

- downloading and verifying the signed release
- verified
- waiting until no client is attached and no agent is busy (with the reason,
  for example the session in which an agent is working)
- installing on the device, after which the app restarts
- needs confirmation on the device. Android asks on the device: on Android 10
  or 11, when the setting is **Ask on this screen**, or when Android's limit of
  one silent update every 30 seconds applies. Open Minion and choose **Install
  update**.
- done: the Version column shows the new version once the Worker reconnects.
- failed, with the reason. The old version keeps running.

**Sessions restart.** Installing a new APK stops every process of the app:
tmux, every shell and every program running in them. Sessions marked **Keep
after restart** come back with their name, folder and start command, but not
the programs that were running. The Update confirmation on the Hub says how
many sessions will restart. **Update** waits for an idle moment. **Update now**
installs at once.

## Who controls updates

- **On the Hub, per Worker** (administrators, on the Worker's page, **App
  updates**):
  - **Automatic.** The Hub sends each new Hub release by itself. The app
    installs it at the next idle moment. If an attempt fails, the Hub does not
    repeat it for the same release.
  - **Manual.** This is the default. The Worker updates only when an
    administrator presses **Update**.
  - **Off.** The Hub refuses to update this Worker, through the API as well.

  Every change is written to the Hub's audit log
  (`worker_app_update_set`). So is every update request
  (`worker_update`), and for automatic updates the start, the install and a
  failure (detail `auto=true`).
- **On the device** (the Minion screen, **Updates**):
  - **Automatic.** This is the default.
  - **Ask on this screen.** Android asks for confirmation on the Minion
    screen.
  - **Off.** The app installs nothing, and the Worker stops offering updates
    (`remote_update=off`). The device setting always wins over the Hub.

## Turn automatic updates off

- For one Worker, from the Hub: on the Worker's page, set **App updates** to
  **Manual** or **Off**.
- On the device: set **Updates** to **Off** on the Minion screen.
- Both at once: set **Off** on the device, which the Hub cannot override.

## Checks before anything is installed

1. The Worker verifies the release's Sigstore signature: `release.yml` for a
   `vX.Y.Z` release, `android-release.yml` for an APK-only `android-vX.Y.Z`
   release. It checks the APK's SHA-256 against the signed `checksums.txt`. It
   refuses an older or the same version.
2. The app runs `minion worker apk-verify` again, offline. It requires exactly
   those bytes while it streams the APK into the install session, the package
   `cc.nasz.minion`, a higher version code, and exactly one signer: the
   pinned release certificate (`android/signing-cert.sha256`).
3. Android itself refuses an APK with another signer or a lower version code.

## Limits

- A silent update needs Android 12 or newer. Android 10 and 11 always show one
  system dialog per update.
- **Install unknown apps** must stay allowed for Minion. A device policy that
  forbids unknown sources blocks updates.
- The dialog can appear only while the Minion screen is open (Android limits
  activity starts from the background). The app also shows a notification.
- No downgrade. To go back, release a higher version built from good source
  and install it, or install it by hand with
  `adb install -r minion-android-<version>.apk`. Uninstalling loses the
  pairing.
- A debug build, or one signed with another key, cannot update itself.
- Updates need the same signing key forever. Keep the key backup.
- Play Protect may scan or stop an update. This is not tested on every device.
- The app does not yet count failed Worker starts after an update. If a new
  version does not start, install a fixed version by hand.

Back to the [README](../../README.md).
