#!/usr/bin/env bash
# Install the pinned SDK manager and the requested SDK packages into a new directory.
# Usage: install-sdk.sh <sdk-dir> <package>...
set -euo pipefail

[ "$#" -ge 2 ] || { echo "usage: $0 <sdk-dir> <package>..." >&2; exit 2; }
mkdir -p "$1"
sdk="$(cd "$1" && pwd)"
shift
tools="$sdk/cmdline-tools/latest"
[ ! -e "$tools" ] || { echo "install-sdk: command-line tools already exist in $sdk" >&2; exit 1; }
work="$(mktemp -d)"
trap 'rm -rf "${work:?}"' EXIT

url=https://dl.google.com/android/repository/commandlinetools-linux-13114758_latest.zip
sha=7ec965280a073311c339e571cd5de778b9975026cfcbe79f2b1cdcb1e15317ee
curl -fsSL --connect-timeout 20 --max-time 300 --retry 2 -o "$work/tools.zip" "$url"
echo "$sha  $work/tools.zip" | sha256sum --check --quiet -
unzip -q "$work/tools.zip" -d "$work"
mkdir "$sdk/cmdline-tools"
mv "$work/cmdline-tools" "$tools"
(
  # sdkmanager closes the pipe once the licenses are accepted.
  set +o pipefail
  yes | "$tools/bin/sdkmanager" --sdk_root="$sdk" --licenses >/dev/null
)
"$tools/bin/sdkmanager" --sdk_root="$sdk" "$@"
