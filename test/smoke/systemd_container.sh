#!/usr/bin/env bash
# systemd_container.sh: shared by deb.sh and rpm.sh (Story 9.7). Source it
# after common.sh.
#
# Deferred (tracked by the GitHub issue "Install smoke (deb/rpm): enroll the
# Worker through an invitation key and assert Connection: connected"):
# minion-worker@smokeuser is configured with the test Hub but stays "idle (not
# enrolled)" and never contacts it, because Worker-side enrollment (Stories
# 4.6/4.7) is not on main. Step to add before v0.1.0-rc.1: enroll through an
# invitation key and assert "Connection: connected".
#
# A clean distribution container with systemd as PID 1, so that the real
# package, its unit and "systemctl" are exercised. The containers are
# privileged on a throw-away CI runner; they never touch the host's tmux or
# systemd.

# sd_start BASE_IMAGE PLATFORM PACKAGES ASSETS_DIR: builds a systemd image from
# BASE_IMAGE with the extra PACKAGES (space separated, installed with the
# package manager named by SD_PM, apt or dnf), starts it and waits until
# systemd is up. Sets SD_CID and SD_IMG.
sd_start() {
	local base=$1 platform=$2 pkgs=$3 assets=$4 install
	case "${SD_PM:?SD_PM must be apt or dnf}" in
	apt) install="apt-get update -qq && apt-get install -y --no-install-recommends systemd systemd-sysv dbus ca-certificates curl procps sudo $pkgs && rm -rf /var/lib/apt/lists/*" ;;
	dnf) install="dnf install -y --setopt=install_weak_deps=False systemd procps-ng sudo shadow-utils $pkgs && dnf clean all" ;;
	*) smoke_die "unknown package manager $SD_PM" ;;
	esac
	SD_IMG="minion-smoke-$SD_PM-${platform#linux/}-$$"
	printf 'FROM %s\nRUN %s\nSTOPSIGNAL SIGRTMIN+3\nCMD ["/sbin/init"]\n' "$base" "$install" |
		docker build --platform "$platform" -t "$SD_IMG" - >/dev/null
	SD_CID=$(docker run -d --platform "$platform" --privileged --cgroupns=host \
		--tmpfs /run --tmpfs /run/lock \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		-v "$assets:/assets:ro" "$SD_IMG")
	smoke_log "waiting for systemd in $SD_CID ($base, $platform)"
	smoke_wait_for 240 "systemd to finish booting" sd_booted
}

sd_booted() {
	local s
	s=$(docker exec "$SD_CID" systemctl is-system-running 2>/dev/null || true)
	[ "$s" = running ] || [ "$s" = degraded ]
}

# sd_exec COMMAND...: runs a command as root in the container.
sd_exec() { docker exec "$SD_CID" "$@"; }

# sd_sh SCRIPT: runs an sh script as root in the container.
sd_sh() { docker exec "$SD_CID" sh -ec "$1"; }

sd_cleanup() {
	[ -z "${SD_CID:-}" ] || docker rm -f "$SD_CID" >/dev/null 2>&1 || true
	[ -z "${SD_IMG:-}" ] || docker rmi -f "$SD_IMG" >/dev/null 2>&1 || true
}

# sd_check_worker USER: the shared part of the deb and rpm smoke tests, run
# once the package is installed. Checks "minion version" (the binary and the mn
# link), starts a test Hub (the installed binary, plain HTTP on loopback, a
# throw-away admin password from a file), starts minion-worker@USER pointed at
# it and checks "minion worker status". SMOKE_TAG is the expected release tag.
sd_check_worker() {
	local user=$1 out status
	out=$(sd_exec minion version)
	smoke_expect_version "$out" "$SMOKE_TAG"
	out=$(sd_exec mn version)
	smoke_expect_version "$out" "$SMOKE_TAG"
	sd_sh 'command -v tmux >/dev/null' || smoke_die "tmux was not installed as a dependency of the package"
	sd_sh 'test -f /usr/lib/systemd/system/minion-worker@.service' || smoke_die "the package installed no minion-worker@.service"

	# The test Hub.
	sd_sh 'umask 077; head -c 24 /dev/urandom | od -An -tx1 | tr -d " \n" >/root/admin.pw; mkdir -p /var/lib/smoke-hub'
	sd_sh 'systemd-run --unit=smoke-hub \
		--setenv=MINION_DATA_DIR=/var/lib/smoke-hub --setenv=MINION_TLS=off \
		--setenv=MINION_LISTEN=127.0.0.1:8443 --setenv=MINION_PUBLIC_URL=http://127.0.0.1:8443 \
		--setenv=MINION_ADMIN_PASSWORD_FILE=/root/admin.pw \
		/usr/bin/minion hub serve'
	smoke_wait_for 60 "the test Hub to answer /healthz" \
		docker exec "$SD_CID" sh -c 'curl -fsS http://127.0.0.1:8443/healthz | grep -q ok'

	# The Worker unit for USER, pointed at the test Hub.
	sd_sh "id -u $user >/dev/null 2>&1 || useradd -m $user"
	sd_sh "mkdir -p /etc/systemd/system/minion-worker@$user.service.d &&
		printf '[Service]\\nEnvironment=MINION_SERVER=http://127.0.0.1:8443\\n' >/etc/systemd/system/minion-worker@$user.service.d/smoke.conf &&
		systemctl daemon-reload &&
		systemctl enable --now minion-worker@$user"
	smoke_wait_for 30 "minion-worker@$user to be active" sd_exec systemctl is-active --quiet "minion-worker@$user"
	# It must stay up (a crash loop restarts every 5 seconds).
	sleep "${SMOKE_SETTLE_SECONDS:-8}"
	sd_exec systemctl is-active --quiet "minion-worker@$user" ||
		smoke_die "minion-worker@$user did not stay active: $(sd_exec journalctl -u "minion-worker@$user" --no-pager -n 20 2>&1 | tail -n 20)"

	# "minion worker status" as the Worker's user asks the running Worker over
	# its local socket; an unpaired Worker is "idle (not enrolled)".
	local i st
	for i in $(seq 1 30); do
		st=$(docker exec "$SD_CID" su -s /bin/sh "$user" -c 'minion worker status --server http://127.0.0.1:8443' 2>&1) &&
			printf '%s\n' "$st" | grep -Eq '^Connection: +(idle|connecting|connected|reconnecting)' && break
		[ "$i" -lt 30 ] || smoke_die "timed out after 30s waiting for minion worker status to reach the running Worker
--- last status output:
$st
--- su environment (runtime and temp dirs):
$(docker exec "$SD_CID" su -s /bin/sh "$user" -c 'id; env | grep -E "^(XDG_RUNTIME_DIR|TMPDIR|HOME)=" ; ls -la /tmp /run/user 2>&1 | head -40' 2>&1)
--- sockets:
$(docker exec "$SD_CID" sh -c 'find /tmp /run/user -name worker.sock -exec ls -la {} \; 2>&1; ls -lad /tmp/minion-* /run/user/*/minion 2>&1' 2>&1)
--- unit environment:
$(sd_exec systemctl show "minion-worker@$user" -p Environment -p User -p PrivateTmp 2>&1)
--- journal:
$(sd_exec journalctl -u "minion-worker@$user" --no-pager -n 40 2>&1 | tail -n 40)"
		sleep 1
	done
	status=$(sd_exec su -s /bin/sh "$user" -c 'minion worker status --server http://127.0.0.1:8443')
	printf '%s\n' "$status" | grep -Eq '^Hub URL: +http://127\.0\.0\.1:8443' ||
		smoke_die "worker status does not show the test Hub URL: $status"
	printf '%s\n' "$status" | grep -Eq "^Version: +$SMOKE_TAG\$" ||
		smoke_die "worker status shows another version than $SMOKE_TAG: $status"
	smoke_log "worker status ok: $(printf '%s\n' "$status" | grep '^Connection:' | sed 's/  */ /g')"

	# A package removal stops cleanly.
	sd_exec systemctl stop "minion-worker@$user"
}
