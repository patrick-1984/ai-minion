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
printf '#!/bin/sh\necho 1000\n' >"$WORK/installed/id"
chmod +x "$WORK/installed/id"
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
	kind=${2:-plain}
	d="$REL/$v"
	mkdir -p "$d"
	: >"$d/checksums.txt"
	for p in linux_amd64 linux_arm64 linux_armv7 darwin_amd64 darwin_arm64; do
		b="$WORK/build/$v-$p"
		mkdir -p "$b"
		if [ "$kind" = join ]; then
			# "worker up" records its arguments and its standard input.
			# shellcheck disable=SC2016 # the fake binary expands its own variables
			printf '#!/bin/sh\nif [ "$1 $2" = "worker up" ]; then\n  if [ "$5" = "--prepare-join" ]; then\n    cat >"$JOINLOG.prepared"\n    if [ -n "${FAKE_DECLINE:-}" ]; then\n      echo "minion: join skipped: existing Worker kept"\n    else\n      echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"\n    fi\n    exit 0\n  fi\n  printf "%%s\\n" "$@" >"$JOINLOG.argv"\n  cat >"$JOINLOG.stdin"\n  exit "${FAKE_RC:-0}"\nfi\necho "fake-minion %s %s $*"\n' "$v" "$p" >"$b/minion"
		else
			printf '#!/bin/sh\necho "fake-minion %s %s $*"\n' "$v" "$p" >"$b/minion"
		fi
		chmod 755 "$b/minion"
		echo licence >"$b/LICENSE"
		name="minion_${v#v}_$p.tar.gz"
		tar -czf "$d/$name" -C "$b" minion LICENSE
		printf '%s  %s\n' "$(sha "$d/$name")" "$name" >>"$d/checksums.txt"
	done
}
make_release v1.2.0
make_release v1.3.0
make_release v1.6.0 join
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
	# shellcheck disable=SC2086 # SCRIPT_ARGS is split into words on purpose
	OUT=$(env PATH="$(fake_uname "$us" "$um"):$WORK/installed:$PATH" HOME="$WORK/home" \
		MINION_RELEASE_BASE="file://$REL" MINION_RELEASE_API="file://$API" \
		MINION_INSTALL_DIR="$rd" MINION_VERSION= "$@" sh "$SCRIPT" $SCRIPT_ARGS 2>&1)
	RC=$?
}

# ---------------------------------------------------------------- the cases
SCRIPT_ARGS=

run_install "$WORK/i1" Linux x86_64
assert_contains "latest release, linux amd64: exit 0" "rc=$RC" "rc=0"
assert_contains "latest release is resolved" "$OUT" "minion_1.3.0_linux_amd64.tar.gz (v1.3.0)"
assert_contains "minion version is printed" "$OUT" "fake-minion v1.3.0 linux_amd64 version"

# An isolated PATH contains exactly one package manager and no real sudo.
depbin="$WORK/depbin"
mkdir "$depbin"
for tool in sh env tar gzip curl awk sed head grep mktemp cp chmod mv mkdir rm cat; do
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
assert_contains "missing tmux does not block the install" "rc=$RC" "rc=0"

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

# --------------------------------------------------------------------- --join
TOKEN="mnj1-AAAAAAAA.BBBBBBBB-secret-token-text"
JL="$WORK/joinlog"
SCRIPT_ARGS=--join
rm -f "$JL".*
run_install "$WORK/j1" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL"
assert_contains "--join installs and joins: exit 0" "rc=$RC" "rc=0"
assert_contains "--join announces the share default" "$OUT" "initial sharing: all"
want_argv='worker
up
--join
-
--join-consent
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
--share-existing
all'
if [ "$(cat "$JL.argv" 2>/dev/null)" = "$want_argv" ]; then ok "--join runs: worker up --join - --join-consent (decision) --share-existing all"; else bad "--join arguments" "$(cat "$JL.argv" 2>/dev/null)"; fi
if [ "$(cat "$JL.stdin" 2>/dev/null)" = "$TOKEN" ]; then ok "the token arrives on standard input"; else bad "the token arrives on standard input" "$(cat "$JL.stdin" 2>/dev/null)"; fi
case "$(cat "$JL.argv")$OUT" in
*"$TOKEN"*) bad "the token is in no argument list and no output" ;;
*) ok "the token is in no argument list and no output" ;;
esac
assert_contains "--join ran the installed binary by absolute path" "$OUT" "Installed $WORK/j1/minion"

# A declined staged preflight leaves the prior binary untouched and does not join.
rm -f "$JL".*
run_install "$WORK/i1" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" FAKE_DECLINE=1
assert_contains "decline exits successfully" "rc=$RC" "rc=0"
assert_contains "decline keeps the prior binary" "$("$WORK/i1/minion" version)" "fake-minion v1.2.0"
assert_no_file "decline does not join" "$JL.argv"
case "$OUT" in *"install.sh: finished"*) bad "decline reports a join" ;; *) ok "decline omits success summary" ;; esac

rm -f "$JL".*
SCRIPT_ARGS="--join --share later"
run_install "$WORK/j2" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" MINION_SHARE_EXISTING=all JOINLOG="$JL"
assert_contains "--share later is passed on" "$(cat "$JL.argv" 2>/dev/null | tr '\n' ' ')" "--share-existing later"
SCRIPT_ARGS="--join --share everything"
run_install "$WORK/j2b" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL"
assert_contains "a bad --share is refused" "$OUT" "--share must be all, none or later"
assert_no_file "a bad --share installs nothing" "$WORK/j2b/minion"

SCRIPT_ARGS=--join
rm -f "$JL".*
run_install "$WORK/j3" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" FAKE_RC=3
assert_contains "a failing join keeps its exit status" "rc=$RC" "rc=3"

run_install "$WORK/j4" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="not-a-token" JOINLOG="$JL"
assert_contains "something that is not a token is refused" "$OUT" "not a join token"
case "$OUT" in *"not-a-token"*) bad "the refusal repeats the input" ;; *) ok "the refusal does not repeat the input" ;; esac
assert_no_file "nothing is installed for a bad token" "$WORK/j4/minion"

# Without a token and without a terminal there is nothing to ask on.
if command -v setsid >/dev/null 2>&1; then
	OUT=$(env PATH="$(fake_uname Linux x86_64):$WORK/installed:$PATH" HOME="$WORK/home" MINION_RELEASE_BASE="file://$REL" \
		MINION_INSTALL_DIR="$WORK/j5" MINION_VERSION=v1.6.0 setsid -w sh "$SCRIPT" --join </dev/null 2>&1)
	assert_contains "no token and no terminal: refused" "$OUT" "needs the join token"
	assert_no_file "nothing is installed without a token" "$WORK/j5/minion"
else
	printf 'skip the no-terminal case (setsid is not installed)\n'
fi

# root is refused: the Worker is the user's.
mkdir "$WORK/rootid"
printf '#!/bin/sh\necho 0\n' >"$WORK/rootid/id"
chmod +x "$WORK/rootid/id"
run_install "$WORK/j6" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" PATH="$WORK/rootid:$(fake_uname Linux x86_64):$WORK/installed:$PATH"
assert_contains "--join as root is refused" "$OUT" "without sudo"
assert_no_file "nothing is installed as root" "$WORK/j6/minion"

# A tampered or truncated archive is rejected before the binary exists, so the
# token is never handed over.
for bad in tampered truncated; do
	nv=v1.7.$([ "$bad" = tampered ] && echo 0 || echo 1)
	mkdir -p "$REL/$nv"
	for f in "$REL"/v1.6.0/*.tar.gz; do cp "$f" "$REL/$nv/$(basename "$f" | sed "s/1\.6\.0/${nv#v}/")"; done
	sed "s/1\.6\.0/${nv#v}/" "$REL/v1.6.0/checksums.txt" >"$REL/$nv/checksums.txt"
	a="$REL/$nv/minion_${nv#v}_linux_amd64.tar.gz"
	if [ "$bad" = tampered ]; then printf 'evil' >>"$a"; else head -c 100 "$a" >"$a.cut" && mv "$a.cut" "$a"; fi
	rm -f "$JL".*
	run_install "$WORK/j-$bad" Linux x86_64 MINION_VERSION="$nv" MINION_JOIN="$TOKEN" JOINLOG="$JL"
	assert_contains "$bad archive refused with --join" "$OUT" "checksum mismatch"
	assert_no_file "$bad archive: nothing installed" "$WORK/j-$bad/minion"
	assert_no_file "$bad archive: the join never ran" "$JL.argv"
done

# --name reaches "worker up" as two arguments; the signed checksums digest is enforced.
rm -f "$JL".*
SCRIPT_ARGS="--join --name build-1"
run_install "$WORK/n1" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL"
assert_contains "--name is passed to worker up" "$(tr '\n' ' ' <"$JL.argv")" "--share-existing all --name build-1"
SCRIPT_ARGS=--join
rm -f "$JL".*
run_install "$WORK/n2" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" MINION_CHECKSUMS_SHA256="$(sha "$REL/v1.6.0/checksums.txt")"
assert_contains "the signed checksums digest is accepted" "rc=$RC" "rc=0"
assert_contains "a finished join says so" "$OUT" "install.sh: finished"
rm -f "$JL".*
run_install "$WORK/n3" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" MINION_CHECKSUMS_SHA256="$(printf 'x' | { sha256sum 2>/dev/null || shasum -a 256; } | cut -d' ' -f1)"
assert_contains "a checksums.txt that is not the signed one is refused" "$OUT" "does not match the signed release"
assert_no_file "…nothing installed" "$WORK/n3/minion"
assert_no_file "…and the join never ran" "$JL.argv"
# A stream cut right after "main" runs nothing.
rm -f "$JL".*
sed '$s/.*/main/' "$SCRIPT" | env PATH="$(fake_uname Linux x86_64):$WORK/installed:$PATH" HOME="$WORK/home" MINION_RELEASE_BASE="file://$REL" \
	MINION_INSTALL_DIR="$WORK/n4" MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" sh -s -- --join >/dev/null 2>&1
assert_no_file "a stream cut right after main installs nothing" "$WORK/n4/minion"

# The simple one-liner pipes the script into sh: "sh -s -- --join" passes the
# flag, the token still arrives on stdin of the binary only (stdin of sh is the
# script), and a pipe cut before the last line runs nothing.
SCRIPT_ARGS=
rm -f "$JL".*
OUT=$(cat "$SCRIPT" | env PATH="$(fake_uname Linux x86_64):$WORK/installed:$PATH" HOME="$WORK/home" MINION_RELEASE_BASE="file://$REL" \
	MINION_INSTALL_DIR="$WORK/p1" MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" sh -s -- --join 2>&1)
if [ "$(cat "$JL.stdin" 2>/dev/null)" = "$TOKEN" ] && [ "$(sed -n 3p "$JL.argv")" = "--join" ]; then ok "piped: sh -s -- --join joins, token on stdin of the binary"; else bad "piped join" "$OUT"; fi
case "$(cat "$JL.argv")$OUT" in *"$TOKEN"*) bad "piped: the token is in argv or output" ;; *) ok "piped: the token is in no argument list and no output" ;; esac
rm -f "$JL".*
OUT=$(sed '$d' "$SCRIPT" | env PATH="$(fake_uname Linux x86_64):$WORK/installed:$PATH" HOME="$WORK/home" MINION_RELEASE_BASE="file://$REL" \
	MINION_INSTALL_DIR="$WORK/p2" MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" sh -s -- --join 2>&1)
assert_no_file "piped, truncated before the last line: nothing installed" "$WORK/p2/minion"
assert_no_file "piped, truncated: the join never ran" "$JL.argv"
head -c 3000 "$SCRIPT" | env PATH="$(fake_uname Linux x86_64):$WORK/installed:$PATH" HOME="$WORK/home" MINION_RELEASE_BASE="file://$REL" \
	MINION_INSTALL_DIR="$WORK/p3" MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" sh -s -- --join >/dev/null 2>&1
assert_no_file "piped, cut mid-script: nothing installed" "$WORK/p3/minion"
SCRIPT_ARGS=--join

# Missing tmux stops a join before anything is installed or used.
run_install "$WORK/j7" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL" PATH="$depbin"
assert_contains "--join without tmux stops" "$OUT" "tmux is needed for a Worker"
assert_no_file "--join without tmux installs nothing" "$WORK/j7/minion"

# An unknown option is refused; without --join nothing joins.
SCRIPT_ARGS=--bogus
run_install "$WORK/j8" Linux x86_64 MINION_VERSION=v1.6.0
assert_contains "unknown option refused" "$OUT" "unknown option '--bogus'"
SCRIPT_ARGS=
rm -f "$JL".*
run_install "$WORK/j9" Linux x86_64 MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" JOINLOG="$JL"
assert_no_file "without --join nothing joins" "$JL.argv"

# The whole script runs from its last line: a copy cut short runs nothing.
sed '$d' "$SCRIPT" >"$WORK/cut.sh"
OUT=$(env HOME="$WORK/home" MINION_RELEASE_BASE="file://$REL" MINION_INSTALL_DIR="$WORK/cut" MINION_VERSION=v1.6.0 sh "$WORK/cut.sh" 2>&1)
assert_no_file "a script without its last line installs nothing" "$WORK/cut/minion"

# Compose enrolls through a disposable container, then runs only credential-free services.
mkdir "$WORK/compose" "$WORK/dockerbin"
printf '#!/bin/sh\nexit 0\n' >"$REL/v1.6.0/bootstrap.sh"
cat >"$WORK/dockerbin/docker" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$DOCKER_LOG"
if [ "$2" = run ]; then cat >"$DOCKER_LOG.stdin"; fi
EOF
chmod +x "$WORK/dockerbin/docker"
real_docker=$(command -v docker || true)
compose_out=$(cd "$WORK/compose" && env PATH="$WORK/dockerbin:$(fake_uname Linux x86_64):$WORK/installed:$PATH" \
	MINION_RELEASE_BASE="file://$REL" MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" DOCKER_LOG="$WORK/docker.log" \
	sh "$SCRIPT" --compose --name 'OrangePi' --share none 2>&1)
assert_contains "Compose runs configure-only via stdin" "$(cat "$WORK/docker.log")" "worker up --configure-only --join - --share-existing none --name OrangePi"
assert_contains "Compose starts in the background" "$(cat "$WORK/docker.log")" "compose up -d"
assert_contains "Compose receives the token on stdin" "$(cat "$WORK/docker.log.stdin")" "$TOKEN"
assert_contains "Compose prints the project directory" "$compose_out" "minion-worker-orangepi"
project="$WORK/compose/minion-worker-orangepi"
case "$(cat "$project/compose.yaml" "$WORK/docker.log")$compose_out" in
*"$TOKEN"*) bad "Compose keeps the token out of files, arguments and output" ;;
*) ok "Compose keeps the token out of files, arguments and output" ;;
esac
if [ -n "$real_docker" ] && "$real_docker" compose version >/dev/null 2>&1; then
	if "$real_docker" compose -f "$project/compose.yaml" config -q; then ok "generated Compose file validates"; else bad "generated Compose file validates"; fi
fi
compose_out=$(cd "$WORK/compose" && env PATH="$WORK/dockerbin:$(fake_uname Linux x86_64):$WORK/installed:$PATH" \
	MINION_RELEASE_BASE="file://$REL" MINION_VERSION=v1.6.0 MINION_JOIN="$TOKEN" DOCKER_LOG="$WORK/docker.log" \
	sh "$SCRIPT" --compose --name 'OrangePi' 2>&1)
assert_contains "an existing Compose project is untouched" "$compose_out" "minion-worker-orangepi already exists"

if command -v shellcheck >/dev/null 2>&1; then
	if SC=$(shellcheck -s sh "$SCRIPT" "$0" 2>&1); then ok "shellcheck"; else bad "shellcheck" "$SC"; fi
else
	printf 'skip shellcheck (not installed)\n'
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
