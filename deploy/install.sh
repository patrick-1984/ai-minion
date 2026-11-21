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
#   MINION_RELEASE_BASE, MINION_RELEASE_API
#                       where archives and the latest release are looked up
#                       (mirrors, tests)
#
# With --join the installer then sets the machine up as a Worker, for the user
# who runs it (no sudo): it runs the installed binary by its absolute path as
#
#     minion worker up --join - --interactive-replace --non-interactive-share <share>
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
set -eu

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

# join_worker runs the installed binary by its absolute path. The token goes in
# on its standard input (printf is a shell builtin: no argument list, no file);
# the terminal stays free for the question about an existing Worker.
join_worker() {
	rc=0
	printf 'Joining the Hub as a Worker (it shares %s sessions first; change that in the Hub)\n' "$share"
	printf '%s\n' "$tok" | "$dir/minion" worker up --join - --interactive-replace --non-interactive-share "$share" || rc=$?
	tok=
	return "$rc"
}

# read_token sets tok from MINION_JOIN or a masked prompt on the terminal.
read_token() {
	tok=${MINION_JOIN:-}
	unset MINION_JOIN
	if [ -z "$tok" ]; then
		( : </dev/tty ) 2>/dev/null || die "--join needs the join token: set MINION_JOIN, or run this in a terminal to be asked for it"
		printf 'Join token (from the Hub'"'"'s Add worker page; what you type is hidden): ' >/dev/tty
		old=$(stty -g </dev/tty 2>/dev/null) || old=
		trap '[ -z "$old" ] || stty "$old" </dev/tty; exit 130' INT TERM
		stty -echo </dev/tty 2>/dev/null || true
		IFS= read -r tok </dev/tty || tok=
		[ -z "$old" ] || stty "$old" </dev/tty 2>/dev/null || true
		trap - INT TERM
		printf '\n' >/dev/tty
	fi
	case $tok in
	mnj1-*) ;;
	*) die "that is not a join token: it starts with mnj1-. Copy the whole token from the Hub's Add worker page" ;;
	esac
}

main() {
	join=0
	share=all
	while [ $# -gt 0 ]; do
		case $1 in
		--join) join=1 ;;
		--share)
			[ $# -ge 2 ] || die "--share needs all, none or later"
			share=$2
			shift
			;;
		--share=*) share=${1#--share=} ;;
		*) die "unknown option '$1' (supported: --join, --share all|none|later)" ;;
		esac
		shift
	done
	case $share in
	all | none | later) ;;
	*) die "--share must be all, none or later" ;;
	esac
	if [ "$join" = 1 ]; then
		[ "$(id -u)" != 0 ] || die "--join sets the Worker up for the user who runs it: run it without sudo, as that user"
		read_token
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
	case "$ver" in
	v[0-9]*.[0-9]*.[0-9]*) ;;
	*) die "invalid MINION_VERSION '$ver': give an exact version such as v0.8.1" ;;
	esac

	archive="minion_${ver#v}_${plat}.tar.gz"
	base="${MINION_RELEASE_BASE:-https://github.com/$REPO/releases/download}/$ver"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	printf 'Downloading %s (%s)\n' "$archive" "$ver"
	curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "could not download $base/$archive"
	curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "could not download $base/checksums.txt"

	want=$(awk -v n="$archive" '$2 == n || $2 == "*" n {print $1; exit}' "$tmp/checksums.txt")
	[ -n "$want" ] || die "$archive is not listed in checksums.txt of $ver"
	got=$(sha256 "$tmp/$archive")
	[ "$got" = "$want" ] || die "checksum mismatch for $archive: got $got, checksums.txt says $want; nothing was installed"

	tar -xzf "$tmp/$archive" -C "$tmp" minion || die "could not unpack minion from $archive"
	install_tmux
	if [ "$join" = 1 ]; then
		command -v tmux >/dev/null 2>&1 || die "tmux is needed for a Worker and could not be installed; install tmux and run this again (nothing was joined, the join token was not used)"
	fi

	dir=${MINION_INSTALL_DIR:-}
	if [ -z "$dir" ]; then
		if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
			dir=/usr/local/bin
		else
			dir=$HOME/.local/bin
		fi
	fi
	mkdir -p "$dir" || die "cannot create $dir"
	# Copy to a unique file next to the target, then rename: a running minion
	# keeps its old file, and two installs at once do not share a staging file.
	new=$(mktemp "$dir/.minion.XXXXXX") || die "cannot write to $dir (run as root, or set MINION_INSTALL_DIR)"
	trap 'rm -rf "$tmp"; rm -f "$new"' EXIT
	cp "$tmp/minion" "$new" || die "cannot write to $dir (run as root, or set MINION_INSTALL_DIR)"
	chmod 0755 "$new" || die "cannot make $new executable"
	mv -f "$new" "$dir/minion" || die "cannot replace $dir/minion"

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
		join_worker
	fi
}

main "$@"
