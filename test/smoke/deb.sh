#!/usr/bin/env bash
# deb.sh: install the exact release deb on a clean Debian 13 container with
# systemd and check it (Story 9.7, AD-24, R11).
#
# Usage: test/smoke/deb.sh ASSETS_DIR TAG [amd64|arm64]
#
# ASSETS_DIR holds the verified release assets (fetch.sh). arm64 runs under
# QEMU (docker run --platform linux/arm64; the workflow installs binfmt).
# Checks: apt installs the deb (and its tmux dependency), "minion version",
# minion-worker@<user> starts against a test Hub and "minion worker status"
# answers.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"
# shellcheck source=test/smoke/systemd_container.sh
. "$here/systemd_container.sh"

[ $# -ge 2 ] || smoke_die "usage: deb.sh ASSETS_DIR TAG [amd64|arm64]"
assets=$(cd "$1" && pwd)
SMOKE_TAG=$2
arch=${3:-amd64}
smoke_require_tag "$SMOKE_TAG"
case "$arch" in amd64 | arm64) ;; *) smoke_die "unsupported deb architecture '$arch'" ;; esac

deb=$(smoke_one_file "$assets" "minion_*_${arch}.deb")
smoke_log "deb smoke test: $(basename "$deb") on debian:13 ($arch)"

export SD_PM=apt
trap sd_cleanup EXIT
sd_start debian:13 "linux/$arch" "" "$assets"
sd_sh "cat /etc/os-release | grep -Fx 'VERSION_ID=\"13\"'" >/dev/null || smoke_die "the container is not Debian 13"
sd_sh "apt-get update -qq && apt-get install -y /assets/$(basename "$deb")" >/dev/null
sd_check_worker smokeuser
smoke_log "deb smoke test passed ($arch)"
