#!/usr/bin/env bash
# Sign a verified unsigned APK with the dedicated release key.
# Usage: sign-apk.sh <unsigned.apk> <version> <signed.apk>
# Set MINION_ANDROID_KEYSTORE and MINION_ANDROID_PASSWORD_FILE to private files.
set -euo pipefail
umask 077

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
[ "$#" = 3 ] || { echo "usage: $0 <unsigned.apk> <version> <signed.apk>" >&2; exit 2; }
tools="${ANDROID_HOME:-${ANDROID_SDK_ROOT:?set ANDROID_HOME or ANDROID_SDK_ROOT}}/build-tools/35.0.0"
keystore="${MINION_ANDROID_KEYSTORE:?set MINION_ANDROID_KEYSTORE}"
password="${MINION_ANDROID_PASSWORD_FILE:?set MINION_ANDROID_PASSWORD_FILE}"
work="$(mktemp -d)"
trap 'rm -rf "${work:?}"' EXIT

"$root/scripts/android/verify-apk.sh" --unsigned "$1" "$2"
"$tools/zipalign" -f -P 16 4 "$1" "$work/aligned.apk"
"$tools/apksigner" sign --ks "$keystore" --ks-type PKCS12 --ks-key-alias minion-release \
  --ks-pass "file:$password" \
  --out "$work/signed.apk" "$work/aligned.apk"
"$root/scripts/android/verify-apk.sh" "$work/signed.apk" "$2"
install -D -m 0644 "$work/signed.apk" "$3"
