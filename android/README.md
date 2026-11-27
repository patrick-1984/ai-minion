# Minion Worker for Android

Minion runs a Worker on the Android device and connects it to a Minion Hub.
The device hosts terminal sessions. Open them from the Hub using an account
authorized to operate this Worker.

## Sideload and connect

1. Download `minion-android-<version>.apk` and verify its digest against the
   signed release checksums. The same APK contains `arm64-v8a` and
   `armeabi-v7a` executables and requires Android 10 (API 29) or newer.
2. Open the APK on the device or run `adb install -r minion-android-<version>.apk`
   against the intended device. Find **Minion Worker** in the phone or Android
   TV launcher.
3. For a join token, copy a token from **Workers > Add worker** on the Hub,
   choose **Use a join token** in the app and paste the token. For a TV remote,
   choose **Pair with a Hub**, enter the full Hub URL and compare the Hub key
   words shown on both screens. Enter the device code in **Workers > Add
   worker**, then approve the Worker on the Hub.
4. To create a shell, enable hidden sessions in **Account > Sessions** if
   your Hub allows that choice. The device's terminal server is private to the
   app. Its sessions count as hidden, and a Hub administrator can disable them.

The Android app asks you to review and accept usage monitoring when you
pair or enter a join token. On Hubs that support it, the Hub administrator
and Worker owner can see usage for every session, including unshared
sessions. That includes session names, folders, models, timing, token counts
and usage limits. This grants no access to terminal content or controls.
On an existing enrollment, **Review usage monitoring** shows the Hub
fingerprint, registration and owner before you accept locally.

**Pause** disconnects from the Hub and keeps the terminal sessions running.
**Resume** reconnects. **Stop Worker** disconnects and closes its terminal
sessions. Closing the screen leaves a running Worker in the background:
**OK, keep running in the background** and Back (Escape on a keyboard) move
the screen away and say so; they never stop the Worker. After the first
pairing or join the app asks once **Start Minion when this device turns
on?** (**Yes, start automatically** is focused for the D-pad, **No** keeps
it off) and then moves to the background. The same choice stays on the
screen as **Start Minion automatically when this device turns on**; on some
devices it takes effect after the first unlock. Android
or the device vendor may suspend or stop the service. A system force-stop
requires opening the app and starting the Worker again. The Worker's Go
connection uses its own TLS and proxy settings.

**Hub address override.** On a network whose DNS cannot resolve the Hub's
name, enter the Hub's IP address (for example `10.3.3.6`) in the optional
**Advanced: Hub address override** field of **Pair with a Hub** or **Use a
join token**. The app takes the hostname from the Hub URL or the join token
and passes `MINION_HUB_DIAL_ADDRESS=<hostname>=<address>` to the Worker on
every start, so it survives Worker, app and device restarts. Only the dial
address changes, with the same port: TLS SNI, the certificate name check and
the HTTP Host stay the hostname, and other hosts and a configured proxy are
dialled as before. The status screen shows the active override; **Clear Hub
address override** (while the Worker is stopped) and **Forget this Hub**
remove it. Only the Android build of the Worker reads this variable.

## File transfer

The app creates the private folder `Minion files` in the Worker's home
folder (`~/Minion files` in a terminal session, mode 0700, inside the app
sandbox, no storage permission). Every file transfer of this device uses
that folder and nothing outside it: a session working in `Minion files` or
below it sends and receives in its own folder, and every other session,
including one in `~` or a `su` shell elsewhere, sends and receives in
`Minion files` itself. Files pushed to an Android session land there, and
the Hub shows the folder as `Minion files/...`. The checkbox **Allow file
transfer (Minion files folder)** next to **Share terminal sessions with the
Hub** is on by default and runs `minion worker config set file_transfer on`
or `off`. Off refuses every later request with `files_disabled` and stops a
transfer that has not been published yet; the Hub cannot turn it back on.
The line under the checkbox shows the folder path, and a value of
`send_only` or `receive_only` set from a terminal session. When the Worker
finds no safe way to confine transfers on the device, it does not offer
file transfer and the line says **Not supported on this device**.

## Rooted devices

The Worker, terminal server and initial shells run under the Minion app UID.
If you use a root manager and have granted Minion Worker access there, you
can invoke the manager's elevation command, such as `su`, inside a live
terminal session. Manage or revoke the grant in the root manager. A grant can
apply to every terminal session under the app UID, so give shell access only
to operators you trust with that grant. The same APK works on a device with
no root manager.

## Build and sign

Use JDK 17 on PATH and the Android SDK platform 35 and build-tools 35.0.0.
The Gradle wrapper pins the Gradle version and distribution checksum. Run
these commands from the repository root.

```sh
scripts/android/build-native.sh --fetch-ndk --version v0.17.0 --out /tmp/minion-native
scripts/android/build-apk.sh v0.17.0 /tmp/minion-native /tmp/minion-unsigned.apk
```

The native output directory must be new or empty. The build verifies the
pinned NDK and source archives, then checks the Bionic DNS import and
16 KiB ELF alignment for both Worker executables. The packaging script
compares every packaged native executable with the verified build output.

The release workflow builds the unsigned APK, then signs it in a separate
runner before release checksums are generated. The repository secrets
`MINION_ANDROID_KEYSTORE_BASE64` and `MINION_ANDROID_KEYSTORE_PASSWORD` hold
the dedicated PKCS12 release key and its password. Keep an encrypted owner
backup of the key and password. Installing an update over an existing copy
requires the same signing identity. The certificate SHA-256 is published in
[signing-cert.sha256](signing-cert.sha256).

For a local release signing run, set `MINION_ANDROID_KEYSTORE` and
`MINION_ANDROID_PASSWORD_FILE` to the private input files.

```sh
scripts/android/sign-apk.sh /tmp/minion-unsigned.apk v0.17.0 /tmp/minion-android-0.17.0.apk
scripts/android/verify-apk.sh /tmp/minion-android-0.17.0.apk v0.17.0
```

## Device checks

Before validating a projector or any older Android device, identify its
model and record the API level and application ABIs.

```sh
adb shell getprop ro.product.model
adb shell getprop ro.build.version.sdk
adb shell getprop ro.product.cpu.abilist
adb shell getconf PAGE_SIZE
```

Use the exact signed APK on an ARM64 phone, a true 32-bit ARMv7 device, the
target Android TV, and an ARM64 device or emulator with 16 KiB pages.
Join a scratch Hub by hostname and approve the Worker, create a shell from a
normal opted-in account, then type and read output. Verify the policy denial
with hidden sessions disabled. Use remote-only D-pad navigation on the TV,
confirm key words and the code are shown in full, and finish the pairing.
Test pause, resume, stop, backgrounding, restart after boot and an in-place
same-signer upgrade. If root access has been granted outside the app, verify
a user-entered elevation command inside a pane while a second pane still
starts as the app UID.
