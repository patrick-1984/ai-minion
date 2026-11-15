#!/bin/sh
# Copy the C sources into the OpenWrt package directory (the SDK builds from the package directory only).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
rm -rf "$here/minion-thin/src"
mkdir -p "$here/minion-thin/src"
cp -R "$here/../core" "$here/../linux" "$here/minion-thin/src/"
rm -rf "$here/minion-thin/src/linux/build" "$here/minion-thin/src/core/tests"
