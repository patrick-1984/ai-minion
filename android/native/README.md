# Native build for the Android app

The [Android Worker app](../README.md) uses a pinned native build. `scripts/android/build-native.sh` builds, for
`arm64-v8a` and `armeabi-v7a`:

- `libminion.so`: the Worker (`cmd/minion`), `CGO_ENABLED=1`, NDK clang, cgo
  resolver, PIE, 16 KiB aligned;
- `libtmux.so`: tmux 3.8 with libevent and ncurses linked in (needs only
  `libc`, `libm` and `libdl`);
- `libminion-launch.so`: a small launcher that ties the native children to the
  Android service process;
- `terminfo/`: the terminfo database the app points `TERMINFO` at;
- `licenses/`: notices from the pinned archives.

Everything is pinned in `versions.lock` (NDK revision and SHA-256, native API
floor, source hashes). `patches/` holds the zero-fuzz patches for tmux (none are
needed for 3.8; `patches/README.md` records the Termux review). Run it locally:

    scripts/android/build-native.sh --fetch-ndk --out /tmp/android-native

`scripts/android/verify-elf.sh <abi> <file>...` checks the result; CI runs both
in `.github/workflows/android-native.yml`.

See the app guide for verification on an installed device.
