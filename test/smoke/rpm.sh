#!/usr/bin/env bash
# rpm.sh: install the exact release rpm on a clean Fedora container with
# systemd and check it (Story 9.7, AD-24, R11).
#
# Usage: test/smoke/rpm.sh ASSETS_DIR TAG
#
# Checks: dnf installs the rpm (and its tmux dependency), "minion version",
# minion-worker@<user> starts against a test Hub and "minion worker status"
# answers. The rpm is the x86_64 one (the runner is amd64).
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"
# shellcheck source=test/smoke/systemd_container.sh
. "$here/systemd_container.sh"

[ $# -eq 2 ] || smoke_die "usage: rpm.sh ASSETS_DIR TAG"
assets=$(cd "$1" && pwd)
SMOKE_TAG=$2
smoke_require_tag "$SMOKE_TAG"

rpm=$(smoke_one_file "$assets" "minion*.x86_64.rpm")
smoke_log "rpm smoke test: $(basename "$rpm") on fedora"

export SD_PM=dnf
trap sd_cleanup EXIT
sd_start fedora:latest linux/amd64 "" "$assets"
sd_sh "grep -Fx 'ID=fedora' /etc/os-release" >/dev/null || smoke_die "the container is not Fedora"
sd_sh "dnf install -y /assets/$(basename "$rpm")" >/dev/null
sd_check_worker smokeuser
smoke_log "rpm smoke test passed"
