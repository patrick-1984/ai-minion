#!/bin/sh
# Run the device installers against isolated release assets and simulated USB tools.
set -eu

here=$(CDPATH='' cd -- "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/v0.17.0" "$work/thin-v0.1.0-preview.1"
export PATH="$work/bin:$PATH" DEV_LOG="$work/log" DEV_ROOT="$work"
export MINION_RELEASE_BASE="file://$work" MINION_THIN_RELEASE_BASE="file://$work"

cat >"$work/bin/adb" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >>"$DEV_LOG"
case "$*" in
devices) printf 'List of devices attached\nscratch-device\tdevice\nother-device\toffline\n' ;;
*-state) echo device ;;
*ro.product.cpu.abilist) echo "${DEV_ABI:-arm64-v8a,armeabi-v7a}" ;;
*ro.build.version.sdk) echo 34 ;;
esac
SH
cat >"$work/bin/uvx" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >>"$DEV_LOG"
case "$*" in
*flash_id) printf 'Chip is ESP32-C3 (revision v0.3)\nDetected flash size: 4 MB\n' ;;
*read_flash*) for out do :; done; cp "$DEV_ROOT/boot.bin" "$out" ;;
esac
SH
chmod +x "$work/bin/adb" "$work/bin/uvx"

apk=minion-android-0.17.0.apk
printf 'test APK\n' >"$work/v0.17.0/$apk"
(cd "$work/v0.17.0" && sha256sum "$apk" >checksums.txt)

sh "$here/android.sh" --version v0.17.0
grep -q '^-s scratch-device install -r .*minion-android-0.17.0.apk$' "$DEV_LOG"
grep -q '^-s scratch-device shell am start -n cc.nasz.minion/.MainActivity$' "$DEV_LOG"
: >"$DEV_LOG"
if DEV_ABI=x86_64 sh "$here/android.sh" --version v0.17.0; then exit 1; fi
if grep -q ' install ' "$DEV_LOG"; then exit 1; fi
printf 'tamper\n' >>"$work/v0.17.0/$apk"
if sh "$here/android.sh" --version v0.17.0; then exit 1; fi
if grep -q ' install ' "$DEV_LOG"; then exit 1; fi

thin="$work/thin-v0.1.0-preview.1"
factory=minion-thin_0.1.0-preview.1_esp32c3_factory-0x0.bin
app=minion-thin_0.1.0-preview.1_esp32c3_app-0x10000.bin
python3 - "$thin/$factory" "$work/boot.bin" <<'PY'
from pathlib import Path
import sys
Path(sys.argv[1]).write_bytes(b"\0" * 0x10001)
Path(sys.argv[2]).write_bytes(b"\xff" * 0x10000)
PY
printf 'test app\n' >"$thin/$app"
(cd "$thin" && sha256sum "$factory" "$app" >SHA256SUMS)
: >"$DEV_LOG"
sh "$here/esp32.sh" --port scratch-port
grep -q 'write_flash 0x0 .*_factory-0x0.bin$' "$DEV_LOG"
: >"$DEV_LOG"
if sh "$here/esp32.sh" --port scratch-port --update; then exit 1; fi
if grep -q 'write_flash' "$DEV_LOG"; then exit 1; fi

head -c 65536 "$thin/$factory" >"$work/boot.bin"
sh "$here/esp32.sh" --port scratch-port --update
grep -q 'write_flash 0x10000 .*_app-0x10000.bin$' "$DEV_LOG"
: >"$DEV_LOG"
if sh "$here/esp32.sh" --port scratch-port; then exit 1; fi
if grep -q 'write_flash' "$DEV_LOG"; then exit 1; fi
