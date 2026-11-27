#!/usr/bin/env bash
# Validate package identity, launchers, architecture coverage and signed bytes.
# Usage: verify-apk.sh [--unsigned] [--device-control] <apk> <version>
#   --device-control  the APK is a device control build (-PminionDeviceControl=true, the gate RC): it must have
#                     libminion_spake2.so per ABI, the BoringSSL notice, the provenance provider and
#                     SYSTEM_ALERT_WINDOW. Without it the APK must have none of them (device control off).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
unsigned=0
device_control=0
while [ "$#" -gt 2 ]; do
  case "$1" in
    --unsigned) unsigned=1; shift ;;
    --device-control) device_control=1; shift ;;
    *) break ;;
  esac
done
[ "$#" = 2 ] || { echo "usage: $0 [--unsigned] [--device-control] <apk> <version>" >&2; exit 2; }
apk="$1"
version="${2#v}"
tools="${ANDROID_HOME:-${ANDROID_SDK_ROOT:?set ANDROID_HOME or ANDROID_SDK_ROOT}}/build-tools/35.0.0"
die() { echo "verify-apk: $*" >&2; exit 1; }

badging="$("$tools/aapt" dump badging "$apk")"
package="$(sed -n "s/^package: name='\([^']*\)' versionCode='\([0-9]*\)' versionName='\([^']*\)'.*/\1 \2 \3/p" <<<"$badging")"
read -r package_id version_code actual_version <<<"$package"
[ "$package_id" = cc.nasz.minion ] && [ "$version" = "$actual_version" ] && [ "$version_code" -gt 0 ] ||
  die "expected cc.nasz.minion version $version"
grep -Fxq "sdkVersion:'29'" <<<"$badging" || die "minSdk must match the native floor"
grep -Fxq "targetSdkVersion:'35'" <<<"$badging" || die "targetSdk must be 35"
grep -Fq "launchable-activity: name='cc.nasz.minion.MainActivity'" <<<"$badging" || die "missing phone launcher"
grep -Fq "leanback-launchable-activity: name='cc.nasz.minion.MainActivity'" <<<"$badging" || die "missing TV launcher"
manifest="$("$tools/aapt" dump xmltree "$apk" AndroidManifest.xml)"
grep -Eq 'android:extractNativeLibs.*0xffffffff' <<<"$manifest" || die "native executables must be extracted"
grep -Eq 'android:allowBackup.*0x0$' <<<"$manifest" || die "app backup must be disabled"
entries="$(unzip -Z1 "$apk")"
expected="$(for abi in arm64-v8a armeabi-v7a; do
  libs="libminion-launch.so libminion.so libtmux.so"
  [ "$device_control" = 1 ] && libs="$libs libminion_spake2.so"
  for lib in $libs; do
    echo "lib/$abi/$lib"
  done
done | LC_ALL=C sort)"
actual="$(grep -E '^lib/[^/]+/[^/]+[.]so$' <<<"$entries" | LC_ALL=C sort)"
[ "$actual" = "$expected" ] || die "native ABI contents differ from the expected package layout"
for notice in minion-LICENSE.txt tmux-COPYING.txt libevent-LICENSE.txt ncurses-COPYING.txt; do
  grep -Fxq "assets/licenses/$notice" <<<"$entries" || die "missing license notice $notice"
done
grep -Fxq 'assets/terminfo/s/screen-256color' <<<"$entries" || die "missing bundled terminfo"
if [ "$device_control" = 1 ]; then
  grep -Fxq 'assets/licenses/boringssl-LICENSE.txt' <<<"$entries" || die "missing license notice boringssl-LICENSE.txt"
  grep -Fq "uses-permission: name='android.permission.SYSTEM_ALERT_WINDOW'" <<<"$badging" ||
    die "device control build without SYSTEM_ALERT_WINDOW"
  grep -Fq 'cc.nasz.minion.adbprobe' <<<"$manifest" || die "device control build without the provenance provider"
else
  if grep -Fq 'SYSTEM_ALERT_WINDOW' <<<"$badging"; then die "device control is off, but the APK asks for SYSTEM_ALERT_WINDOW"; fi
  if grep -Fq 'cc.nasz.minion.adbprobe' <<<"$manifest"; then die "device control is off, but the APK has the provenance provider"; fi
  if grep -Fxq 'assets/licenses/boringssl-LICENSE.txt' <<<"$entries"; then die "device control is off, but the APK has its notice"; fi
fi

if [ "$unsigned" = 0 ]; then
  "$tools/zipalign" -c -P 16 4 "$apk"
  signer="$("$tools/apksigner" verify --verbose --print-certs "$apk")"
  expected_cert="$(cat "$root/android/signing-cert.sha256")"
  actual_cert="$(sed -n 's/^Signer #[0-9]* certificate SHA-256 digest: //p' <<<"$signer")"
  [ "$actual_cert" = "$expected_cert" ] || die "APK signer is different from the published certificate"
  grep -Eq '^Verified using v[23] scheme .*: true$' <<<"$signer" ||
    die "APK signature scheme verification failed"
  echo "APK signer SHA-256 $actual_cert"
fi
echo "Verified $package_id $actual_version ($version_code), arm64-v8a and armeabi-v7a"
