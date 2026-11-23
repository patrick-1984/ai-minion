#!/bin/sh
# install.sh: the one-line installer of the minion binary for Linux and macOS.
#
#     curl -fsSL https://github.com/patrick-1984/ai-minion/releases/latest/download/install.sh | sh
#
# It finds the latest release (or MINION_VERSION), downloads the archive for
# this machine (linux or darwin; amd64, arm64 or, on Linux, armv7) and the
# release's checksums.txt, checks the archive's sha256 against checksums.txt,
# and installs "minion" into /usr/local/bin when that directory is writable
# (as root, for example), otherwise into ~/.local/bin. It then prints
# "minion version". If tmux is missing, it installs tmux with the local
# package manager. Set up the Worker with "minion worker up".
#
# The archive is checked against checksums.txt of the same release, both
# fetched over HTTPS from GitHub. The signature of checksums.txt is not checked
# here: to verify it too, follow the manual install in docs/install/hub-binary.md
# ("Get the binary"). Later updates with "minion update" verify it.
#
# Environment (all optional):
#   MINION_VERSION      the release to install (v0.8.1 or 0.8.1); default: latest
#   MINION_INSTALL_DIR  where to put minion instead of the default above
#   MINION_CHECKSUMS_SHA256
#                       the SHA-256 of the signed checksums.txt of the release
#                       (the Hub's verified command passes it); a checksums.txt
#                       with another digest is refused
#   MINION_RELEASE_BASE, MINION_RELEASE_API
#                       where archives and the latest release are looked up
#                       (mirrors, tests)
#
# With --join the installer then sets the machine up as a Worker, for the user
# who runs it (no sudo): it runs the installed binary by its absolute path as
#
#     minion worker up --join - --share-existing <share>
#
# with the join token from the Hub's Add worker page on the standard input of
# that one process: from MINION_JOIN (read once, then removed from the
# environment) or, without it, asked for at a masked prompt on /dev/tty. The
# token is in no argument list and in no file. If a Worker is already set up
# for this user, "minion worker up" shows it and asks on the terminal whether to
# replace it (default: no, nothing changes and the exit status is 0). Without
# --join the installer only installs the binary, as before.
#
#     --join            install, then join the Hub of the token as a Worker
#     --service         install a Linux system service for the invoking user
#     --root            install and run the Worker as root on Linux
#     --compose         create a Docker Compose project in this directory
#     --replace / --keep  answer the existing Worker question without a prompt
#     --accept-usage    accept the Hub's usage disclosure for this enrollment
#     --name NAME       the Worker's name in the Hub (only with --join)
#     --share all|none|later
#                       what the new Worker shares first (default: all; change
#                       it in the Hub). Only with --join.
#
# The Hub's Add worker page offers a command that downloads this script to a
# private file and checks its SHA-256 against the signed release manifest before
# running it. The whole script runs from the last line, so a download cut short
# never runs half of it.
#
# Needs: curl, tar, uname, and sha256sum or shasum.
REPO=patrick-1984/ai-minion

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

# warn reports a problem that does not stop the installation: tmux is only
# needed on a machine that runs a Worker, a Hub-only install works without it.
warn() {
	printf 'install.sh: warning: %s\nContinuing without tmux: a Worker needs it, a Hub does not.\n' "$*" >&2
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "neither sha256sum nor shasum is installed"
	fi
}

install_tmux() {
	command -v tmux >/dev/null 2>&1 && return 0
	if command -v apt-get >/dev/null 2>&1; then
		set -- env DEBIAN_FRONTEND=noninteractive apt-get install -y tmux
	elif command -v dnf >/dev/null 2>&1; then set -- dnf install -y tmux
	elif command -v yum >/dev/null 2>&1; then set -- yum install -y tmux
	elif command -v apk >/dev/null 2>&1; then set -- apk add --no-cache tmux
	elif command -v pacman >/dev/null 2>&1; then set -- pacman -S --noconfirm --needed tmux
	elif command -v zypper >/dev/null 2>&1; then set -- zypper --non-interactive install tmux
	elif command -v brew >/dev/null 2>&1; then set -- brew install tmux
	else { warn "tmux is missing. Install tmux from https://github.com/tmux/tmux/wiki/Installing and run the installer again."; return 0; }
	fi
	if [ "$1" != brew ] && [ "$(id -u)" != 0 ]; then
		command -v sudo >/dev/null 2>&1 || { warn "tmux is missing and sudo is unavailable. As root, run '$*', then rerun the installer."; return 0; }
		set -- sudo -n "$@"
	elif [ "$1" = brew ] && [ "$(id -u)" = 0 ]; then
		[ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] || { warn "Homebrew needs a regular user. Run 'brew install tmux' as the Homebrew owner, then rerun the installer."; return 0; }
		set -- sudo -n -H -u "$SUDO_USER" "$(command -v brew)" install tmux
	fi
	printf 'Installing tmux with %s\n' "$*"
	# Minimal Debian images often have no package index yet.
	if command -v apt-get >/dev/null 2>&1; then
		if [ "$(id -u)" = 0 ]; then apt-get update; else sudo -n apt-get update; fi ||
			{ warn "cannot refresh the apt package index. Run 'sudo apt-get update && sudo apt-get install tmux', then rerun the installer."; return 0; }
	fi
	"$@" </dev/null || { warn "tmux installation failed. Run '$(printf '%s' "$*" | sed 's/^sudo -n /sudo /')' in a terminal."; return 0; }
	command -v tmux >/dev/null 2>&1 || { warn "the package manager finished but tmux is missing from PATH. Open a new terminal and rerun the installer."; return 0; }
}

# platform prints "<os>_<arch>" as in the archive names of a release.
platform() {
	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported system '$(uname -s)': use Linux or macOS (Windows: install.ps1)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l | armv7 | armv8l) arch=armv7 ;;
	*) die "unsupported architecture '$(uname -m)' (supported: amd64, arm64, and armv7 on Linux)" ;;
	esac
	[ "$os/$arch" != darwin/armv7 ] || die "unsupported architecture armv7 on macOS"
	printf '%s_%s\n' "$os" "$arch"
}

# Compose enrolls once with the token on stdin. Its durable project contains
# only the image recipe and the Worker state and cache, with no join token.
compose_worker() (
	if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
		die "--compose requires Docker with the Compose plugin"
	fi
	project_name=minion-worker-${name:-minion}
	case $project_name in *[!a-zA-Z0-9._-]*) die "--compose needs a name with letters, digits, dots, dashes or underscores" ;; esac
	project_name=$(printf '%s' "$project_name" | tr '[:upper:]' '[:lower:]' | tr . _)
	umask 077
	mkdir "$project_name" || die "$project_name already exists or cannot be created. Use a different --name or directory"
	project_dir=$PWD/$project_name
	cd "$project_name"
	ready=0
	trap 'if [ "$ready" = 0 ]; then rm -f bootstrap.sh compose.yaml; cd ..; rmdir "$project_name"; fi' EXIT
	curl -fsSL -o bootstrap.sh "$base/bootstrap.sh" || die "could not download $base/bootstrap.sh"
	[ -s bootstrap.sh ] || die "downloaded bootstrap.sh is empty"
	cat >compose.yaml <<EOF
name: $project_name
services:
  minion-worker:
    build:
      context: .
      dockerfile_inline: |
        FROM alpine:3.22
        RUN apk add --no-cache tmux
    entrypoint: ["sh", "/bootstrap.sh"]
    command: ["worker", "run"]
    restart: unless-stopped
    environment:
      MINION_VERSION: "$ver"
      MINION_CACHE_DIR: /cache
      MINION_STATE_DIR: /data
      MINION_SESSIONS_VISIBLE: "no"
    volumes:
      - ./bootstrap.sh:/bootstrap.sh:ro
      - worker-data:/data
      - worker-cache:/cache
volumes:
  worker-data:
  worker-cache:
EOF
	ready=1
	if [ -n "$name" ]; then set -- --name "$name"; else set --; fi
	if [ "$accept_usage" = 1 ]; then set -- "$@" --accept-usage; fi
	printf '%s\n' "$tok" | docker compose run --build --rm -T minion-worker worker up --configure-only --join - --share-existing "$share" "$@" ||
		die "enrollment failed. Inspect the output above and use a new invitation. To discard this project and its data, run docker compose down -v in $project_dir"
	docker compose up -d || die "the Worker is enrolled. To retry starting it, run docker compose up -d in $project_dir"
	docker compose exec -T minion-worker sh /bootstrap.sh worker status --wait ||
		die "the project started. Check Worker startup with docker compose logs minion-worker in $project_dir"
	printf 'Compose project: %s\n  docker compose logs minion-worker\n  docker compose stop minion-worker\n  docker compose start minion-worker\n' "$project_dir"
)

# sudo never receives the token in its arguments or environment. Preflight
# runs a verified executable in a root-only temporary directory.
authorized() {
	if [ "$(id -u)" = 0 ]; then "$@"; else sudo -n -- "$@"; fi
}

# need_systemd MODE: --service and --root install a systemd unit, so refuse
# early (before any download or change) where there is none.
need_systemd() {
	[ "$(uname -s)" = Linux ] || die "--$1 needs Linux with systemd. Use \"Run as me\" instead: run the same command without --$1."
	[ -d "${MINION_SYSTEMD_DIR:-/run/systemd/system}" ] || die "This computer does not run systemd (for example a container such as a Coder workspace), so no system service can be installed here. Use \"Run as me\" instead: run the same command without --$1; it starts the Worker now and again at login."
}

worker_up() {
	bin=$1
	shift
	case $mode in
	service) authorized "$bin" worker up --as-user "$target_user" --join - "$@" ;;
	root) authorized "$bin" worker up --as-user root --join - "$@" ;;
	*) "$bin" worker up --join - "$@" ;;
	esac
}

cleanup() {
	[ -z "$root_tmp" ] || authorized rm -rf -- "$root_tmp"
	rm -rf "$tmp"
	[ -z "$new" ] || rm -f "$new"
}

# check_join asks before installing dependencies or changing the binary.
# worker up returns the consent it recorded, which the actual join rechecks.
check_join() {
	set -- --prepare-join "$@"
	consent=$(printf '%s\n' "$tok" | worker_up "$staged" "$@") || return "$?"
	if [ "$consent" = "minion: join skipped: existing Worker kept" ]; then
		exit 0
	fi
	case $consent in
	*[!0-9a-f]* | '') die "could not get the Worker profile decision; nothing was installed" ;;
	esac
	[ "${#consent}" = 64 ] || die "invalid Worker profile decision; nothing was installed"
}

join_worker() {
	if [ "$accept_usage" = 1 ]; then set -- "$@" --accept-usage; fi
	printf 'Joining the Hub as a Worker (initial sharing: %s; change it in the Hub)\n' "$share"
	printf '%s\n' "$tok" | worker_up "$dir/minion" --join-consent "$consent" --share-existing "$share" "$@" || return "$?"
	tok=
	printf 'install.sh: finished: the Worker is set up (see the summary above).\n'
}

# read_token sets tok from MINION_JOIN or a masked prompt on the terminal.
read_token() {
	if [ -z "$tok" ]; then
		( : </dev/tty ) 2>/dev/null || die "--join needs the join token: set MINION_JOIN, or run this in a terminal to be asked for it"
		old=$(stty -g </dev/tty 2>/dev/null) || die "could not read terminal settings to hide the join token"
		trap 'stty "$old" </dev/tty; exit 130' HUP INT TERM
		stty -echo </dev/tty 2>/dev/null || die "could not hide the join token; nothing was read"
		printf 'Join token (from the Hub Add worker page; typing is hidden): ' >/dev/tty
		IFS= read -r tok </dev/tty || tok=
		stty "$old" </dev/tty || die "could not restore terminal settings"
		trap - HUP INT TERM
		printf '\n' >/dev/tty
	fi
	case $tok in
	mnj1-*) ;;
	*) die "that is not a join token: it starts with mnj1-. Copy the whole token from the Hub's Add worker page" ;;
	esac
}

main() {
	set -eu
	tok=${MINION_JOIN:-}
	unset MINION_JOIN MINION_JOIN_FILE
	[ "${1:-}" = --end ] || die "this script was cut short: download it again"
	shift
	join=0
	mode=user
	share=all
	name=
	decision=
	accept_usage=0
	while [ $# -gt 0 ]; do
		case $1 in
		--join) join=1 ;;
		--accept-usage) accept_usage=1 ;;
		--service | --root | --compose)
			[ "$mode" = user ] || die "choose one of --service, --root or --compose"
			mode=${1#--}
			join=1
			;;
		--replace | --keep)
			[ -z "$decision" ] || die "choose either --replace or --keep"
			decision=$1
			;;
		--share)
			[ $# -ge 2 ] || die "--share needs all, none or later"
			share=$2
			shift
			;;
		--share=*) share=${1#--share=} ;;
		--name)
			[ $# -ge 2 ] || die "--name needs the Worker's name"
			name=$2
			shift
			;;
		*) die "unknown option '$1' (supported: --join, --service, --root, --compose, --share all|none|later, --name NAME, --replace, --keep, --accept-usage)" ;;
		esac
		shift
	done
	case $share in
	all | none | later) ;;
	*) die "--share must be all, none or later" ;;
	esac
	if [ "$accept_usage" = 1 ]; then
		[ "$join" = 1 ] || die "--accept-usage needs an enrollment mode such as --join or --compose"
		printf '%s\n' "Usage monitoring for this enrollment" \
			"The Hub administrator and Worker owner can see usage for every session, including unshared sessions." \
			"Reports may include session names, folders, models, account identifiers, timing, token counts, costs and usage limits." \
			"This grants no terminal content or control access. Sharing and bucket choices do not change usage coverage." >&2
	fi
	if [ "$join" = 1 ]; then
		case $mode in
		user) [ "$(id -u)" != 0 ] || die "run --join without sudo, or give --root to run a root Worker" ;;
		service)
			need_systemd service
			if [ "$(id -u)" = 0 ]; then target_user=${SUDO_USER:-}; else target_user=$(id -un); fi
			[ -n "$target_user" ] && [ "$target_user" != root ] || die "run --service from the account the Worker will use, or use --root"
			;;
		root) need_systemd root ;;
		esac
		read_token
		case $mode in
		service | root)
			if [ "$(id -u)" != 0 ]; then
				command -v sudo >/dev/null 2>&1 || die "--$mode needs sudo"
				sudo -v || die "sudo authorization was declined"
			fi
			;;
		esac
	fi
	command -v curl >/dev/null 2>&1 || die "curl is not installed"
	command -v tar >/dev/null 2>&1 || die "tar is not installed"

	plat=$(platform)
	ver=${MINION_VERSION:-}
	if [ -z "$ver" ]; then
		api=${MINION_RELEASE_API:-https://api.github.com/repos/$REPO/releases}
		ver=$(curl -fsSL "$api/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1) ||
			die "could not look up the latest release at $api/latest"
		[ -n "$ver" ] || die "could not read the latest release from $api/latest"
	fi
	case "$ver" in
	v*) ;;
	*) ver=v$ver ;;
	esac
	printf '%s\n' "$ver" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
		die "invalid MINION_VERSION: give an exact version such as v0.8.1"

	archive="minion_${ver#v}_${plat}.tar.gz"
	base="${MINION_RELEASE_BASE:-https://github.com/$REPO/releases/download}/$ver"
	if [ "$mode" = compose ]; then
		[ -z "$decision" ] || die "--compose creates a new project; omit --replace or --keep"
		compose_worker
		return
	fi
	tmp=$(mktemp -d)
	new=
	root_tmp=
	trap cleanup EXIT

	printf 'Downloading %s (%s)\n' "$archive" "$ver"
	curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "could not download $base/$archive"
	curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "could not download $base/checksums.txt"
	# The Hub's verified command passes the SHA-256 of the signed checksums.txt:
	# a checksums.txt from anywhere else is refused, so the archive it lists is
	# the signed release's archive.
	if [ -n "${MINION_CHECKSUMS_SHA256:-}" ]; then
		[ "$(sha256 "$tmp/checksums.txt")" = "$MINION_CHECKSUMS_SHA256" ] || die "checksums.txt does not match the signed release (expected SHA-256 $MINION_CHECKSUMS_SHA256); nothing was installed"
	fi

	want=$(awk -v n="$archive" '$2 == n || $2 == "*" n {print $1; exit}' "$tmp/checksums.txt")
	[ -n "$want" ] || die "$archive is not listed in checksums.txt of $ver"
	got=$(sha256 "$tmp/$archive")
	[ "$got" = "$want" ] || die "checksum mismatch for $archive: got $got, checksums.txt says $want; nothing was installed"

	tar -xzf "$tmp/$archive" -C "$tmp" minion || die "could not unpack minion from $archive"
	set --
	if [ -n "$name" ]; then set -- --name "$name"; fi
	staged=$tmp/minion
	case $mode in
	service | root)
		root_tmp=$(authorized mktemp -d /var/tmp/minion-worker-install.XXXXXX)
		authorized chmod 0755 "$root_tmp"
		staged=$root_tmp/minion
		authorized install -m 0755 "$tmp/minion" "$staged"
		;;
	esac
	if [ "$join" = 1 ]; then
		if [ -n "$decision" ]; then check_join "$decision" "$@"; else check_join "$@"; fi
	fi
	install_tmux
	if [ "$join" = 1 ]; then
		command -v tmux >/dev/null 2>&1 || die "tmux is needed for a Worker and could not be installed; install tmux and run this again (nothing was joined, the join token was not used)"
	fi

	case $mode in
	service | root)
		dir=/usr/local/bin
		dest=$(authorized mktemp "$dir/.minion.XXXXXX")
		if ! authorized install -m 0755 "$staged" "$dest" || ! authorized mv -f -- "$dest" "$dir/minion"; then
			authorized rm -f -- "$dest"
			die "could not install $dir/minion"
		fi
		;;
	*)
		dir=${MINION_INSTALL_DIR:-}
		if [ -z "$dir" ]; then
			if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
				dir=/usr/local/bin
			else
				dir=$HOME/.local/bin
			fi
		fi
		mkdir -p "$dir" || die "cannot create $dir"
		# Copy to a unique file next to the target, then rename.
		new=$(mktemp "$dir/.minion.XXXXXX") || die "cannot write to $dir (run as root, or set MINION_INSTALL_DIR)"
		cp "$tmp/minion" "$new" || die "cannot write to $dir"
		chmod 0755 "$new" || die "cannot make $new executable"
		mv -f "$new" "$dir/minion" || die "cannot replace $dir/minion"
		;;
	esac

	printf 'Installed %s/minion\n' "$dir"
	case ":${PATH}:" in
	*":$dir:"*) ;;
	*)
		# shellcheck disable=SC2016 # $PATH is printed literally on purpose
		printf 'Note: %s is not on your PATH. Add it, for example:\n  export PATH="%s:$PATH"\n' "$dir" "$dir"
		;;
	esac
	"$dir/minion" version

	if [ "$join" = 1 ]; then
		join_worker "$@"
	fi
}

{ main --end "$@"; } # minion installer complete
