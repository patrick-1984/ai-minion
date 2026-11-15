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
# "minion version". It installs nothing else: tmux comes from your package
# manager, and the Worker is set up with "minion worker up".
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
# Needs: curl, tar, uname, and sha256sum or shasum.
set -eu

REPO=patrick-1984/ai-minion

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
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
