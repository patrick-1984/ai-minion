#!/usr/bin/env bash
# check-headless.sh: the headless `minion` has no GUI dependency.
#  1. No package outside cmd/minion-tray imports a GUI library (go list -deps),
#     for every release target of minion.
#  2. The tray never builds for armv7 (Raspberry Pi): the linux/arm build of
#     cmd/minion-tray links no GUI library, and neither the minion goreleaser
#     config nor the tray workflow builds the tray for arm.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
gui='fyne.io/systray|ncruces/zenity|godbus/dbus|github.com/getlantern/systray|fyne.io/fyne'

fail=0
for t in linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64 darwin/amd64 darwin/arm64; do
  os=${t%/*}; arch=${t#*/}
  # Everything except the tray itself (tests excluded: -test is not set).
  pkgs=$(GOOS=$os GOARCH=$arch GOARM=7 go list ./... | grep -v -e '/cmd/minion-tray$')
  # shellcheck disable=SC2086
  bad=$(GOOS=$os GOARCH=$arch GOARM=7 go list -deps $pkgs | grep -E "$gui" || true)
  if [ -n "$bad" ]; then echo "GUI library in the headless packages ($t):"; echo "$bad"; fail=1; fi
done

bad=$(GOOS=linux GOARCH=arm GOARM=7 go list -deps ./cmd/minion-tray | grep -E "$gui" || true)
if [ -n "$bad" ]; then echo "the linux/arm tray build links a GUI library:"; echo "$bad"; fail=1; fi

# The tray is built by the tray workflow only, never for 32-bit ARM, and never
# by the headless release (goreleaser) or `make cross`.
if grep -q 'cmd/minion-tray' .goreleaser.yaml; then echo ".goreleaser.yaml must not build minion-tray"; fail=1; fi
if grep -q 'minion-tray' <(sed -n '/^cross:/,/^$/p' Makefile); then echo "make cross must not build minion-tray"; fail=1; fi
if grep -Eq 'goarch: *arm$|GOARCH: *arm$|goarm|GOARM' .github/workflows/tray.yml; then echo "tray.yml must not build for 32-bit ARM"; fail=1; fi

# Positive control: the tray build does link the GUI libraries (the check above
# would otherwise pass vacuously).
GOOS=linux GOARCH=amd64 go list -deps ./cmd/minion-tray | grep -F 'fyne.io/systray' >/dev/null || { echo "control failed: tray does not link systray"; fail=1; }
exit $fail
