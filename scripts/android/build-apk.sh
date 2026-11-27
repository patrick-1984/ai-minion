#!/usr/bin/env bash
# Assemble and verify a universal unsigned APK from one verified native build.
# Usage: build-apk.sh <version> <native-build-dir> <output.apk>
# Environment (gate RC only, docs/minion-android-adb/GATE-runbook.md):
#   MINION_DEVICE_CONTROL=true  build with -PminionDeviceControl=true (device control compiled in and on)
#   MINION_BORINGSSL_DIR=DIR    offline checkout of the pinned BoringSSL tag (else CMake downloads it)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
[ "$#" = 3 ] || { echo "usage: $0 <version> <native-build-dir> <output.apk>" >&2; exit 2; }
version="${1#v}"
native="$(cd "$2" && pwd)"
output="$(realpath -m -- "$3")"
work="$(mktemp -d)"
trap 'rm -rf "${work:?}"' EXIT

(cd "$native" && sha256sum --strict --check --quiet SHA256SUMS)
for abi in arm64-v8a armeabi-v7a; do
  mkdir -p "$work/lib/$abi"
  for binary in libminion.so libtmux.so libminion-launch.so; do
    cp "$native/$abi/$binary" "$work/lib/$abi/$binary"
  done
done
mkdir "$work/assets"
cp -R "$native/terminfo" "$native/licenses" "$work/assets/"
cp "$root/LICENSE" "$work/assets/licenses/minion-LICENSE.txt"
cp "$native/native-manifest.txt" "$work/assets/native-manifest.txt"

gradle_extra=()
verify_extra=()
if [ "${MINION_DEVICE_CONTROL:-}" = true ]; then
  gradle_extra+=(-PminionDeviceControl=true)
  verify_extra+=(--device-control)
  if [ -n "${MINION_BORINGSSL_DIR:-}" ]; then gradle_extra+=(-PminionBoringsslDir="$MINION_BORINGSSL_DIR"); fi
fi
"$root/android/gradlew" --no-daemon --stacktrace -p "$root/android" :app:assembleRelease \
  -PminionVersion="$version" -PminionNativePackage="$work" ${gradle_extra[@]+"${gradle_extra[@]}"}
apk="$root/android/app/build/outputs/apk/release/app-release-unsigned.apk"
"$root/scripts/android/verify-apk.sh" --unsigned ${verify_extra[@]+"${verify_extra[@]}"} "$apk" "$version"
for abi in arm64-v8a armeabi-v7a; do
  for lib in "$work/lib/$abi/"*.so; do
    unzip -p "$apk" "lib/$abi/${lib##*/}" | cmp "$lib" -
  done
done
mkdir -p "$(dirname "$output")"
install -m 0644 "$apk" "$output"
