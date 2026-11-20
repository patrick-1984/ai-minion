#!/usr/bin/env bash
# tray-package.sh GOOS GOARCH VERSION: builds the desktop tray and writes
# dist/tray/minion-tray_<version>_<goos>_<goarch>.{tar.gz,zip}.
# GOARCH "universal" (darwin only) builds amd64 and arm64 with cgo and joins
# them with lipo; it needs a macOS host. Everything else is a pure Go build.
set -euo pipefail
goos=$1 goarch=$2 v=${3:-dev}
cd "$(dirname "${BASH_SOURCE[0]}")/.."
mod=github.com/patrick-1984/ai-minion
flags="-s -w -X $mod/internal/version.Version=$v"
name="minion-tray_${v#v}_${goos}_${goarch}"
dir="dist/tray/$name"
rm -rf "$dir"; mkdir -p "$dir"
if [ "$goos" = darwin ]; then
  for a in amd64 arm64; do
    CGO_ENABLED=1 GOOS=darwin GOARCH=$a go build -trimpath -ldflags "$flags" -o "$dir/minion-tray.$a" ./cmd/minion-tray
  done
  lipo -create -output "$dir/minion-tray" "$dir/minion-tray.amd64" "$dir/minion-tray.arm64"
  rm "$dir/minion-tray.amd64" "$dir/minion-tray.arm64"
elif [ "$goos" = windows ]; then
  # -H=windowsgui: no console window behind the tray.
  CGO_ENABLED=0 GOOS=windows GOARCH=$goarch go build -trimpath -ldflags "$flags -H=windowsgui" -o "$dir/minion-tray.exe" ./cmd/minion-tray
else
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags "$flags" -o "$dir/minion-tray" ./cmd/minion-tray
fi
cp LICENSE "$dir/"
if [ "$goos" = windows ]; then
  (cd dist/tray && zip -qr "$name.zip" "$name")
else
  tar -C dist/tray -czf "dist/tray/$name.tar.gz" "$name"
fi
rm -rf "$dir"
ls -l dist/tray
