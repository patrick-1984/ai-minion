#!/bin/sh
# install_test.sh: tests of deploy/install.sh, a POSIX harness like
# bootstrap_test.sh. A fake release tree is served as file:// URLs (curl reads
# them), and a fake uname picks the platform. No network is involved.
#
# Usage: sh deploy/install_test.sh
# Needs: sh, curl, tar, awk, sed, sha256sum or shasum. shellcheck is run when it
# is installed.

set -u

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/install.sh"
PASS=0
FAIL=0
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
# Keep all tests independent of the host's tmux and package managers.
mkdir "$WORK/installed"
printf '#!/bin/sh\nexit 0\n' >"$WORK/installed/tmux"
chmod +x "$WORK/installed/tmux"
for tool in sudo apt-get dnf yum apk pacman zypper brew; do
	printf '#!/bin/sh\nexit 99\n' >"$WORK/installed/$tool"
	chmod +x "$WORK/installed/$tool"
done

ok() {
	PASS=$((PASS + 1))
	printf 'ok   %s\n' "$1"
}
bad() {
	FAIL=$((FAIL + 1))
	printf 'FAIL %s\n' "$1"
	if [ -n "${2:-}" ]; then printf '%s\n' "$2" | sed 's/^/     | /'; fi
}
assert_contains() { # NAME HAYSTACK NEEDLE
	case "$2" in
	*"$3"*) ok "$1" ;;
	*) bad "$1" "missing: $3
in:
$2" ;;
	esac
}
assert_no_file() { # NAME PATH
	if [ ! -e "$2" ]; then ok "$1"; else bad "$1" "exists: $2"; fi
}

sha() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# --------------------------------------------------------------- fake release
REL="$WORK/download"
API="$WORK/api"
mkdir -p "$REL" "$API"

# make_release VERSION: archives for every platform of a release; the "binary"
# prints its version and platform.
make_release() {
	v=$1
	d="$REL/$v"
	mkdir -p "$d"
	: >"$d/checksums.txt"
	for p in linux_amd64 linux_arm64 linux_armv7 darwin_amd64 darwin_arm64; do
		b="$WORK/build/$v-$p"
		mkdir -p "$b"
		printf '#!/bin/sh\necho "fake-minion %s %s $*"\n' "$v" "$p" >"$b/minion"
		chmod 755 "$b/minion"
		echo licence >"$b/LICENSE"
		name="minion_${v#v}_$p.tar.gz"
		tar -czf "$d/$name" -C "$b" minion LICENSE
		printf '%s  %s\n' "$(sha "$d/$name")" "$name" >>"$d/checksums.txt"
	done
}
make_release v1.2.0
make_release v1.3.0
printf '{\n  "id": 1,\n  "tag_name": "v1.3.0",\n  "draft": false\n}\n' >"$API/latest"

# fake_uname S M: a directory with a uname that prints S for -s and M for -m.
fake_uname() {
	fu="$WORK/uname-$1-$2"
	mkdir -p "$fu"
	# shellcheck disable=SC2016 # $1 belongs to the fake uname
	printf '#!/bin/sh\ncase "$1" in -s) echo %s ;; -m) echo %s ;; *) echo %s ;; esac\n' "$1" "$2" "$1" >"$fu/uname"
	chmod 755 "$fu/uname"
	printf '%s\n' "$fu"
}

# run_install DIR UNAME_S UNAME_M [VAR=VALUE...]: runs install.sh into DIR and
# sets OUT (stdout and stderr) and RC.
run_install() {
	rd=$1 us=$2 um=$3
	shift 3
	OUT=$(env PATH="$(fake_uname "$us" "$um"):$WORK/installed:$PATH" HOME="$WORK/home" \
		MINION_RELEASE_BASE="file://$REL" MINION_RELEASE_API="file://$API" \
		MINION_INSTALL_DIR="$rd" MINION_VERSION= "$@" sh "$SCRIPT" 2>&1)
	RC=$?
}

# ---------------------------------------------------------------- the cases
run_install "$WORK/i1" Linux x86_64
assert_contains "latest release, linux amd64: exit 0" "rc=$RC" "rc=0"
assert_contains "latest release is resolved" "$OUT" "minion_1.3.0_linux_amd64.tar.gz (v1.3.0)"
assert_contains "minion version is printed" "$OUT" "fake-minion v1.3.0 linux_amd64 version"

# An isolated PATH contains exactly one package manager and no real sudo.
depbin="$WORK/depbin"
mkdir "$depbin"
for tool in sh env tar gzip curl awk sed head mktemp cp chmod mv mkdir rm; do
	ln -s "$(command -v "$tool")" "$depbin/$tool"
done
if command -v sha256sum >/dev/null 2>&1; then ln -s "$(command -v sha256sum)" "$depbin/sha256sum"
else ln -s "$(command -v shasum)" "$depbin/shasum"; fi
cp "$(fake_uname Linux x86_64)/uname" "$depbin/uname"
cat >"$depbin/id" <<'EOF'
#!/bin/sh
echo 1000
EOF
cat >"$depbin/sudo" <<'EOF'
#!/bin/sh
echo "sudo $*" >>"$DEP_LOG"
[ "$1" = "-n" ] || exit 1
shift
exec "$@"
EOF
chmod +x "$depbin/id" "$depbin/sudo"
for spec in "apt-get install -y" "dnf install -y" "yum install -y" "apk add --no-cache" \
	"pacman -S --noconfirm --needed" "zypper --non-interactive install" "brew install"; do
	# shellcheck disable=SC2086 # split the command for this case
	set -- $spec
	manager=$1
	cat >"$depbin/$manager" <<'EOF'
#!/bin/sh
echo "${0##*/} $*" >>"$DEP_LOG"
[ "$1" = update ] && exit 0
cp "$DEP_TMUX" "$DEP_BIN/tmux"
EOF
	chmod +x "$depbin/$manager"
	: >"$WORK/deps.log"
	run_install "$WORK/deps" Linux x86_64 PATH="$depbin" DEP_LOG="$WORK/deps.log" DEP_BIN="$depbin" DEP_TMUX="$WORK/installed/tmux"
	assert_contains "$manager installs missing tmux" "rc=$RC" "rc=0"
	assert_contains "$manager arguments" "$(cat "$WORK/deps.log")" "$spec tmux"
	if [ "$manager" != brew ]; then assert_contains "$manager uses noninteractive sudo" "$(cat "$WORK/deps.log")" "sudo -n"; fi
	rm -f "$depbin/$manager" "$depbin/tmux"
done
run_install "$WORK/deps" Linux x86_64 PATH="$depbin"
assert_contains "missing package manager explains how to get tmux" "$OUT" "github.com/tmux/tmux/wiki/Installing"

for case in "Linux aarch64 linux_arm64" "Linux arm64 linux_arm64" "Linux armv7l linux_armv7" "Linux armv8l linux_armv7" \
	"Darwin arm64 darwin_arm64" "Darwin x86_64 darwin_amd64"; do
	# shellcheck disable=SC2086 # split the case on purpose
	set -- $case
	run_install "$WORK/arch-$1-$2" "$1" "$2"
	assert_contains "arch mapping $1 $2 -> $3" "$OUT" "fake-minion v1.3.0 $3 version"
done

run_install "$WORK/bad-arch" Linux mips
assert_contains "unknown architecture refused" "$OUT" "unsupported architecture 'mips'"
run_install "$WORK/bad-os" FreeBSD amd64
assert_contains "unknown system refused" "$OUT" "unsupported system 'FreeBSD'"
run_install "$WORK/bad-mac" Darwin armv7l
assert_contains "armv7 on macOS refused" "$OUT" "armv7 on macOS"

run_install "$WORK/pinned" Linux x86_64 MINION_VERSION=1.2.0
assert_contains "MINION_VERSION without v" "$OUT" "fake-minion v1.2.0 linux_amd64 version"
run_install "$WORK/badver" Linux x86_64 MINION_VERSION=latest-ish
assert_contains "invalid MINION_VERSION refused" "$OUT" "invalid MINION_VERSION"

# A tampered archive: the checksum does not match, nothing is installed.
cp -R "$REL/v1.3.0" "$REL/v1.4.0"
for f in "$REL"/v1.4.0/*.tar.gz; do mv "$f" "$(printf '%s' "$f" | sed 's/1\.3\.0/1.4.0/')"; done
sed 's/1\.3\.0/1.4.0/' "$REL/v1.3.0/checksums.txt" >"$REL/v1.4.0/checksums.txt"
printf 'evil' >>"$REL/v1.4.0/minion_1.4.0_linux_amd64.tar.gz"
run_install "$WORK/tampered" Linux x86_64 MINION_VERSION=v1.4.0
assert_contains "checksum mismatch refused" "$OUT" "checksum mismatch"
assert_contains "checksum mismatch exits non-zero" "rc=$RC" "rc=1"
assert_no_file "nothing installed after a mismatch" "$WORK/tampered/minion"

# An archive that checksums.txt does not list.
mkdir -p "$REL/v1.5.0"
cp "$REL/v1.3.0/minion_1.3.0_linux_amd64.tar.gz" "$REL/v1.5.0/minion_1.5.0_linux_amd64.tar.gz"
: >"$REL/v1.5.0/checksums.txt"
run_install "$WORK/unlisted" Linux x86_64 MINION_VERSION=v1.5.0
assert_contains "unlisted archive refused" "$OUT" "not listed in checksums.txt"
assert_no_file "nothing installed for an unlisted archive" "$WORK/unlisted/minion"

# A missing release.
run_install "$WORK/missing" Linux x86_64 MINION_VERSION=v9.9.9
assert_contains "missing release refused" "$OUT" "could not download"

# Default directory: ~/.local/bin when /usr/local/bin is not writable, with a
# PATH hint. (Skipped as root, where /usr/local/bin is writable.)
if [ ! -w /usr/local/bin ]; then
	OUT=$(env PATH="$(fake_uname Linux x86_64):$WORK/installed:/usr/bin:/bin" HOME="$WORK/home" \
		MINION_RELEASE_BASE="file://$REL" MINION_RELEASE_API="file://$API" MINION_VERSION= MINION_INSTALL_DIR= \
		sh "$SCRIPT" 2>&1)
	assert_contains "default dir is ~/.local/bin" "$OUT" "Installed $WORK/home/.local/bin/minion"
	assert_contains "PATH hint" "$OUT" "is not on your PATH"
else
	printf 'skip default directory case (/usr/local/bin is writable)\n'
fi

# An update replaces the binary in place.
run_install "$WORK/i1" Linux x86_64 MINION_VERSION=v1.2.0
assert_contains "reinstall replaces the binary" "$OUT" "fake-minion v1.2.0 linux_amd64 version"
left=$(find "$WORK/i1" -name '.minion.*')
if [ -z "$left" ]; then ok "no staging file is left behind"; else bad "no staging file is left behind" "$left"; fi

if command -v shellcheck >/dev/null 2>&1; then
	if SC=$(shellcheck -s sh "$SCRIPT" "$0" 2>&1); then ok "shellcheck"; else bad "shellcheck" "$SC"; fi
else
	printf 'skip shellcheck (not installed)\n'
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
