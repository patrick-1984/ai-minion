#!/bin/sh
# Post-install script of the minion deb and rpm packages (Story 9.1, AD-24).
#
# - Makes systemd read the new minion-worker@.service template.
# - On an upgrade restarts the Worker instances that already run (the unit uses
#   KillMode=process, so tmux sessions survive a restart).
# - Never enables or starts a new instance: the instance name is a Unix user
#   and "minion worker up" (or systemctl enable --now) chooses it.
#
# deb calls it as "configure [old-version]", rpm as "<number of installed
# versions>" (1 = first install, 2 or more = upgrade).
#
# Test hooks (not for production use): MINION_SYSTEMCTL, MINION_SYSTEMD_RUN_DIR.
set -e

systemctl_cmd="${MINION_SYSTEMCTL:-systemctl}"
run_dir="${MINION_SYSTEMD_RUN_DIR:-/run/systemd/system}"

upgrade=0
case "${1:-}" in
configure)
	[ -n "${2:-}" ] && upgrade=1
	;;
'' | *[!0-9]*) ;;
*)
	[ "$1" -gt 1 ] && upgrade=1
	;;
esac

# Without a running systemd (container build, chroot) there is nothing to do.
if [ -d "$run_dir" ] && command -v "$systemctl_cmd" >/dev/null 2>&1; then
	"$systemctl_cmd" daemon-reload || true
	if [ "$upgrade" = 1 ]; then
		"$systemctl_cmd" try-restart 'minion-worker@*.service' || true
	fi
fi

if [ "$upgrade" = 0 ]; then
	echo "minion is installed. Set up a Worker with: minion worker up --help"
	echo "or enable the unit directly: sudo systemctl enable --now minion-worker@<user>"
fi
exit 0
