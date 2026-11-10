#!/bin/sh
# Pre-remove script of the minion deb and rpm packages (Story 9.1, AD-24).
#
# On a real removal it stops and disables every minion-worker@<user> instance,
# so that no unit is left pointing at a deleted binary. Without a running
# systemd (chroot, offline image) it still disables the enabled instances; it
# only skips stopping them and the daemon-reload. On an upgrade it does
# nothing (the post-install script restarts running instances).
#
# deb calls it as "remove", "upgrade" or "deconfigure", rpm as "<number of
# versions that remain>" (0 = removal, 1 or more = upgrade).
#
# Test hooks (not for production use): MINION_SYSTEMCTL, MINION_SYSTEMD_RUN_DIR,
# MINION_SYSTEMD_ETC_DIR.
set -e

systemctl_cmd="${MINION_SYSTEMCTL:-systemctl}"
run_dir="${MINION_SYSTEMD_RUN_DIR:-/run/systemd/system}"
etc_dir="${MINION_SYSTEMD_ETC_DIR:-/etc/systemd/system}"

removal=0
case "${1:-}" in
remove) removal=1 ;;
'' | *[!0-9]*) ;;
*)
	[ "$1" -eq 0 ] && removal=1
	;;
esac
[ "$removal" = 1 ] || exit 0

# Without a running systemd (chroot, offline image) there is no daemon to talk
# to: units cannot be listed or stopped and there is nothing to reload, but the
# enable symlinks are files and "systemctl disable" removes them offline. They
# must go, otherwise a later reinstall of the unit starts those instances at
# boot without another enable operation.
running=0
if [ -d "$run_dir" ] && command -v "$systemctl_cmd" >/dev/null 2>&1; then
	running=1
fi

{
	if [ "$running" = 1 ]; then
		# Instances that are loaded (running, failed or stopped) ...
		"$systemctl_cmd" list-units --all --plain --no-legend --no-pager \
			'minion-worker@*.service' 2>/dev/null | awk '{print $1}'
	fi
	# ... and instances that are only enabled.
	for link in "$etc_dir"/*.wants/minion-worker@*.service; do
		[ -e "$link" ] || [ -L "$link" ] || continue
		basename "$link"
	done
} | sort -u | while read -r unit; do
	case "$unit" in
	minion-worker@?*.service)
		if [ "$running" = 1 ]; then
			"$systemctl_cmd" disable --now "$unit" || true
		elif command -v "$systemctl_cmd" >/dev/null 2>&1; then
			"$systemctl_cmd" disable "$unit" || true
		fi
		;;
	esac
done
if [ "$running" = 1 ]; then
	"$systemctl_cmd" daemon-reload || true
fi
exit 0
