#!/bin/sh
# bootstrap_test.sh: tests of deploy/bootstrap.sh (Story 9.3), a bats-free POSIX
# harness. It serves a fake release tree from a local HTTP server (busybox httpd,
# or python3 as a fallback), runs the script in a fresh cache for every case and
# checks the result. No real network, no real signature and no tmux is involved.
#
# Usage: sh deploy/bootstrap_test.sh
# It is meant to run in alpine and debian containers (busybox is the server in
# alpine; in debian install busybox-static or python3), but no workflow runs it
# yet: wiring it into .github/workflows/ci.yml is an open integrator item (the
# workflow is not owned by Story 9.3). Needs: sh, curl or wget, tar, awk, sed,
# grep, sha256sum, and busybox or python3 for the server. shellcheck is run when
# it is installed. The wget downloader is exercised too when wget is installed.

set -u

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/bootstrap.sh"
PASS=0
FAIL=0
WORK=$(mktemp -d)
SERVER_PID=""

stop_server() {
	if [ -n "$SERVER_PID" ]; then
		kill "$SERVER_PID" 2>/dev/null
		wait "$SERVER_PID" 2>/dev/null
		SERVER_PID=""
	fi
}
cleanup() {
	stop_server
	rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

ok() {
	PASS=$((PASS + 1))
	printf 'ok   %s\n' "$1"
}
bad() {
	FAIL=$((FAIL + 1))
	printf 'FAIL %s\n' "$1"
	if [ -n "${2:-}" ]; then printf '%s\n' "$2" | sed 's/^/     | /'; fi
}

# check NAME: reads the verdict from the exit status of the following command
# list is awkward in sh, so the helpers below take plain values.
assert_eq() { # NAME WANT GOT
	if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want: $2
got:  $3"; fi
}
assert_contains() { # NAME HAYSTACK NEEDLE
	case "$2" in
	*"$3"*) ok "$1" ;;
	*) bad "$1" "missing: $3
in:
$2" ;;
	esac
}
assert_not_contains() { # NAME HAYSTACK NEEDLE
	case "$2" in
	*"$3"*) bad "$1" "unexpected: $3
in:
$2" ;;
	*) ok "$1" ;;
	esac
}
assert_file() { # NAME PATH
	if [ -e "$2" ]; then ok "$1"; else bad "$1" "missing: $2"; fi
}
assert_dir_exists() { # NAME PATH
	if [ -d "$2" ]; then ok "$1"; else bad "$1" "missing directory: $2"; fi
}
assert_no_file() { # NAME PATH
	if [ ! -e "$2" ]; then ok "$1"; else bad "$1" "exists: $2"; fi
}

sha() { sha256sum "$1" | awk '{print $1}'; }

# --------------------------------------------------------------- fake release
REL="$WORK/site/download"
API="$WORK/site/api"
COSIGN_DIR="$WORK/site/cosign"
mkdir -p "$REL" "$API" "$COSIGN_DIR"

# make_release VERSION: archives for the three architectures. The "binary" is a
# shell script that prints what it is, which arguments it got and whether the
# bootstrap-only variables leaked into its environment.
make_release() {
	v=$1
	d="$REL/$v"
	mkdir -p "$d"
	: >"$d/checksums.txt"
	for a in amd64 arm64 armv7; do
		b="$WORK/build/$v-$a"
		mkdir -p "$b"
		cat >"$b/minion" <<BIN
#!/bin/sh
echo "fake-minion $v $a args:\$*"
env | grep -E '^MINION_(VERSION|SHA256|CACHE_DIR)=' | sed 's/^/leak: /'
exit \${FAKE_MINION_EXIT:-0}
BIN
		chmod 755 "$b/minion"
		echo "licence" >"$b/LICENSE"
		name="minion_${v#v}_linux_$a.tar.gz"
		tar -czf "$d/$name" -C "$b" minion LICENSE
		printf '%s  %s\n' "$(sha "$d/$name")" "$name" >>"$d/checksums.txt"
	done
	echo '{"fake":"sigstore bundle"}' >"$d/checksums.txt.sigstore.json"
}

for v in v1.3.0 v1.4.1 v1.4.2 v1.4.10 v1.5.0 v2.0.0-rc.1; do make_release "$v"; done

# GitHub order of the keys: tag_name ... draft, prerelease. v1.5.0 is flagged as
# a prerelease although its tag looks stable; v2.0.0-rc.1 is a real candidate; a
# draft is never listed by the real API but must be ignored anyway.
cat >"$API/releases" <<'JSON'
[
  {"id": 9, "tag_name": "v2.0.0-rc.1", "name": "v2.0.0-rc.1", "draft": false, "prerelease": true, "author": {"login": "x"}},
  {"id": 8, "tag_name": "v1.5.0", "name": "v1.5.0", "draft": false, "prerelease": true},
  {"id": 7, "tag_name": "v1.4.10", "name": "v1.4.10", "draft": false, "prerelease": false},
  {"id": 6, "tag_name": "v1.4.2", "name": "v1.4.2", "draft": false, "prerelease": false},
  {"id": 5, "tag_name": "v1.4.1", "name": "v1.4.1", "draft": false, "prerelease": false},
  {"id": 4, "tag_name": "v1.9.9", "name": "v1.9.9", "draft": true, "prerelease": false},
  {"id": 3, "tag_name": "v1.3.0", "name": "v1.3.0", "draft": false, "prerelease": false}
]
JSON

# A fake cosign that logs how it was called and checks the bundle and the blob.
cat >"$COSIGN_DIR/cosign-fake" <<'COS'
#!/bin/sh
echo "cosign $*" >>"$COSIGN_LOG"
echo "cosign-env TUF_ROOT=${TUF_ROOT:-} TUF_MIRROR=${TUF_MIRROR:-} TUF_ROOT_JSON=${TUF_ROOT_JSON:-} SIGSTORE_ROOT_FILE=${SIGSTORE_ROOT_FILE:-} SIGSTORE_REKOR_PUBLIC_KEY=${SIGSTORE_REKOR_PUBLIC_KEY:-}" >>"$COSIGN_LOG"
[ "${FAKE_COSIGN_FAIL:-0}" = 1 ] && { echo "Error: none of the expected identities matched" >&2; exit 1; }
bundle="" blob="" troot=""
while [ $# -gt 0 ]; do
	case "$1" in
	--bundle) bundle=$2; shift ;;
	--trusted-root) troot=$2; shift ;;
	--certificate-identity | --certificate-oidc-issuer | --certificate-github-workflow-repository | --certificate-github-workflow-ref) shift ;;
	verify-blob) ;;
	*) blob=$1 ;;
	esac
	shift
done
if [ -n "$troot" ]; then
	# Offline verification with a stored trusted root: no TUF, no network. Only the
	# production root verifies; another root (an attacker's, an unrelated mirror's) does not.
	[ -s "$troot" ] || { echo "Error: the trusted root file is missing" >&2; exit 1; }
	grep -q 'production trusted root' "$troot" || { echo "Error: the trusted root does not verify this bundle" >&2; exit 1; }
else
	# Like cosign v3 with a new-format bundle: needs a writable TUF root, which
	# it fills (default mirror directory, as the real cosign does) with the Sigstore
	# trusted root. The log shows what was in the TUF directory before.
	echo "tufstart: $(ls -A "$TUF_ROOT" 2>/dev/null | tr '\n' ' ')" >>"$COSIGN_LOG"
	[ -n "${TUF_ROOT:-}" ] && mkdir -p "$TUF_ROOT" && [ -w "$TUF_ROOT" ] ||
		{ echo "Error: getting trusted root from TUF for new bundle verification" >&2; exit 1; }
	if [ "${FAKE_NO_ROOT:-0}" != 1 ]; then
		m="${FAKE_MIRROR:-tuf-repo-cdn.sigstore.dev}"
		mkdir -p "$TUF_ROOT/$m/targets"
		echo "{\"fake\":\"${FAKE_ROOT_CONTENT:-production trusted root}\"}" >"$TUF_ROOT/$m/targets/trusted_root.json"
		if [ "${FAKE_EXTRA_ROOT:-0}" = 1 ]; then
			mkdir -p "$TUF_ROOT/other-mirror.example/targets"
			echo '{"fake":"another mirror root"}' >"$TUF_ROOT/other-mirror.example/targets/trusted_root.json"
		fi
	fi
fi
[ -s "$bundle" ] && [ -s "$blob" ] || { echo "Error: missing bundle or blob" >&2; exit 1; }
exit 0
COS
chmod 755 "$COSIGN_DIR/cosign-fake"
for a in amd64 arm64 arm; do cp "$COSIGN_DIR/cosign-fake" "$COSIGN_DIR/cosign-linux-$a"; done
FAKE_COSIGN_SHA=$(sha "$COSIGN_DIR/cosign-fake")

# The script under test, with the pinned cosign digests replaced by the digest of
# the fake: the only way to change a pin is to edit the script itself.
UNDER_TEST="$WORK/bootstrap.sh"
sed -e "s/^COSIGN_SHA256_AMD64=.*/COSIGN_SHA256_AMD64=\"$FAKE_COSIGN_SHA\"/" \
	-e "s/^COSIGN_SHA256_ARM64=.*/COSIGN_SHA256_ARM64=\"$FAKE_COSIGN_SHA\"/" \
	-e "s/^COSIGN_SHA256_ARMV7=.*/COSIGN_SHA256_ARMV7=\"$FAKE_COSIGN_SHA\"/" \
	"$SCRIPT" >"$UNDER_TEST"

# A fake uname, to run the same script as amd64, arm64, armv7 and others.
FAKEBIN="$WORK/fakebin"
mkdir -p "$FAKEBIN"
cat >"$FAKEBIN/uname" <<'UN'
#!/bin/sh
case "$1" in
-s) echo "${FAKE_UNAME_S:-Linux}" ;;
-m) echo "${FAKE_UNAME_M:-x86_64}" ;;
*) /usr/bin/uname "$@" ;;
esac
UN
chmod 755 "$FAKEBIN/uname"

# --------------------------------------------------------------------- server
start_server() {
	stop_server
	i=0
	while [ "$i" -lt 30 ]; do
		PORT=$((20000 + ($$ + i * 7919) % 20000))
		if command -v busybox >/dev/null 2>&1 && busybox httpd --help >/dev/null 2>&1; then
			busybox httpd -f -p "127.0.0.1:$PORT" -h "$WORK/site" >/dev/null 2>&1 &
		else
			python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$WORK/site" >/dev/null 2>&1 &
		fi
		SERVER_PID=$!
		n=0
		while [ "$n" -lt 40 ]; do
			if kill -0 "$SERVER_PID" 2>/dev/null && probe "http://127.0.0.1:$PORT/api/releases"; then
				return 0
			fi
			kill -0 "$SERVER_PID" 2>/dev/null || break
			n=$((n + 1))
			sleep 0.1 2>/dev/null || sleep 1
		done
		stop_server
		i=$((i + 1))
	done
	echo "cannot start a local HTTP server" >&2
	exit 2
}
probe() {
	if command -v curl >/dev/null 2>&1; then curl -fs -o /dev/null "$1"; else wget -q -O /dev/null "$1"; fi
}

# ------------------------------------------------------------------- runners
CASE_N=0
new_case() {
	CASE_N=$((CASE_N + 1))
	CACHE="$WORK/cache$CASE_N"
	COSIGN_LOG="$WORK/cosign$CASE_N.log"
	: >"$COSIGN_LOG"
}

# run_boot ARGS...: the script against the fake server, results in $OUT, $RC.
BOOT_PATH="$FAKEBIN:$PATH"
run_boot() {
	OUT=$(env \
		PATH="$BOOT_PATH" \
		HOME=/nonexistent \
		WGET_LOG="${WGET_LOG:-/dev/null}" \
		MINION_CACHE_DIR="$CACHE" \
		MINION_RELEASE_BASE="http://127.0.0.1:$PORT/download" \
		MINION_RELEASE_API="http://127.0.0.1:$PORT/api/releases" \
		MINION_COSIGN_BASE="http://127.0.0.1:$PORT/cosign" \
		COSIGN_LOG="$COSIGN_LOG" \
		sh "$UNDER_TEST" "$@" 2>&1)
	RC=$?
}

entries() {
	for e in "$CACHE"/v[0-9]*; do
		[ -d "$e" ] && printf '%s\n' "${e##*/}"
	done
}
# entry_dir VERSION: the cache entry directory of that version.
entry_dir() {
	for e in "$CACHE/$1"-*; do
		[ -d "$e" ] && printf '%s\n' "$e" && return 0
	done
	return 1
}
# shellcheck disable=SC2012
stat_of() { ls -li "$1" | awk '{print $1, $5, $6, $7, $8}'; }
entry_count() { entries | grep -c . || true; }

ARCHIVE_142_AMD64="$REL/v1.4.2/minion_1.4.2_linux_amd64.tar.gz"
SHA_142_AMD64=$(sha "$ARCHIVE_142_AMD64")
IDENT="https://github.com/patrick-1984/ai-minion/.github/workflows/release.yml@refs/tags"
ISSUER="https://token.actions.githubusercontent.com"

# =========================================================== AC 1: exact + sha
start_server

new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot hub serve --flag
assert_eq "exact version + checksum: exit 0" 0 "$RC"
assert_contains "exact version + checksum: runs minion with the arguments" "$OUT" "fake-minion v1.4.2 amd64 args:hub serve --flag"
assert_file "exact version + checksum: immutable <version>-<digest> entry holds the verified archive" "$CACHE/v1.4.2-$SHA_142_AMD64/minion_1.4.2_linux_amd64.tar.gz"
assert_eq "exact version + checksum: one entry only" 1 "$(entry_count)"
assert_eq "exact version + checksum: cosign is not used" "" "$(cat "$COSIGN_LOG")"
assert_no_file "exact version + checksum: no cosign downloaded" "$CACHE/tools"
assert_not_contains "exec: bootstrap-only variables are not passed on" "$OUT" "leak:"

new_case
MINION_VERSION=1.4.2 MINION_SHA256="$(printf '%s' "$SHA_142_AMD64" | tr 'a-f' 'A-F')" run_boot version
assert_eq "exact version without the v, upper-case checksum: exit 0" 0 "$RC"
assert_contains "exact version without the v: runs v1.4.2" "$OUT" "fake-minion v1.4.2 amd64 args:version"

new_case
MINION_VERSION=v1.4.2 MINION_SHA256="0000000000000000000000000000000000000000000000000000000000000000" run_boot version
assert_eq "mismatched checksum: refused" 1 "$RC"
assert_contains "mismatched checksum: explains" "$OUT" "checksum mismatch"
assert_not_contains "mismatched checksum: minion is not run" "$OUT" "fake-minion"
assert_eq "mismatched checksum: nothing cached" 0 "$(entry_count)"

new_case
MINION_VERSION=v1.4.2 MINION_SHA256="abc" run_boot version
assert_eq "malformed checksum: refused" 1 "$RC"
assert_contains "malformed checksum: explains" "$OUT" "64 hex digits"

# ===================================================== AC 3: sha + channel
for sel in 1.4 1 latest v1.4; do
	new_case
	stop_server # prove that the refusal needs no network
	MINION_VERSION="$sel" MINION_SHA256="$SHA_142_AMD64" run_boot version
	assert_eq "checksum with channel '$sel': refused" 1 "$RC"
	assert_contains "checksum with channel '$sel': explanation" "$OUT" "allowed only with an exact MINION_VERSION"
	assert_not_contains "checksum with channel '$sel': minion is not run" "$OUT" "fake-minion"
	start_server
done
new_case
MINION_SHA256="$SHA_142_AMD64" run_boot version
assert_eq "checksum with the default selector (latest): refused" 1 "$RC"

# ================================================ AC 1: channel + cosign
new_case
MINION_VERSION=latest run_boot hub serve
assert_eq "latest + cosign: exit 0" 0 "$RC"
assert_contains "latest: newest stable release (numeric sort, no rc, no prerelease, no draft)" "$OUT" "fake-minion v1.4.10 amd64 args:hub serve"
log=$(cat "$COSIGN_LOG")
assert_contains "cosign: verify-blob with the bundle" "$log" "verify-blob --bundle"
assert_contains "cosign: certificate identity = release workflow at refs/tags/<resolved>" "$log" "--certificate-identity $IDENT/v1.4.10 "
assert_contains "cosign: issuer is GitHub Actions" "$log" "--certificate-oidc-issuer $ISSUER"
assert_contains "cosign: source repository claim" "$log" "--certificate-github-workflow-repository patrick-1984/ai-minion "
assert_contains "cosign: source ref claim = refs/tags/<resolved>" "$log" "--certificate-github-workflow-ref refs/tags/v1.4.10 "
assert_contains "cosign: verifies checksums.txt" "$log" "checksums.txt"
assert_contains "cosign: TUF_ROOT lies inside the cache (HOME=/nonexistent)" "$log" "TUF_ROOT=$CACHE/"
assert_no_file "cosign: no TUF directory is kept in the cache (fresh and private per verification)" "$CACHE/tools/sigstore-tuf"
digest=$(sha "$REL/v1.4.10/minion_1.4.10_linux_amd64.tar.gz")
assert_file "latest: immutable <version>-<digest> entry holds the verified archive" "$CACHE/v1.4.10-$digest/minion_1.4.10_linux_amd64.tar.gz"
assert_file "latest: pinned cosign cached per version and arch" "$CACHE/tools/cosign-v3.1.4-amd64"

new_case
MINION_VERSION=v1.4.2 run_boot version
assert_eq "exact version without checksum uses cosign: exit 0" 0 "$RC"
assert_contains "exact version without checksum: cosign identity for that tag" "$(cat "$COSIGN_LOG")" "--certificate-identity $IDENT/v1.4.2 "
assert_contains "exact version without checksum: source ref claim for that tag" "$(cat "$COSIGN_LOG")" "--certificate-github-workflow-ref refs/tags/v1.4.2 "

new_case
TUF_ROOT=/elsewhere TUF_MIRROR=http://evil.invalid TUF_ROOT_JSON=/evil.json SIGSTORE_ROOT_FILE=/evil.pem SIGSTORE_REKOR_PUBLIC_KEY=/evil.pub \
	MINION_VERSION=v1.4.2 run_boot version
assert_eq "inherited trust-root variables: exit 0" 0 "$RC"
log=$(cat "$COSIGN_LOG")
assert_contains "inherited TUF_ROOT is replaced by a private TUF root in the cache" "$log" "cosign-env TUF_ROOT=$CACHE/.tmp."
assert_not_contains "inherited TUF_ROOT is never used" "$log" "TUF_ROOT=/elsewhere"
assert_contains "inherited TUF_MIRROR, TUF_ROOT_JSON and Sigstore key files never reach cosign" "$log" "TUF_MIRROR= TUF_ROOT_JSON= SIGSTORE_ROOT_FILE= SIGSTORE_REKOR_PUBLIC_KEY="

for pair in "1.4:v1.4.10" "1:v1.4.10" "v1.4:v1.4.10" "1.3:v1.3.0" "1.4.1:v1.4.1"; do
	sel=${pair%%:*}
	want=${pair#*:}
	new_case
	MINION_VERSION="$sel" run_boot x
	assert_contains "selector '$sel' resolves to $want" "$OUT" "fake-minion $want amd64"
done

new_case
MINION_VERSION=2 run_boot x
assert_eq "selector 2 (only a release candidate exists): refused" 1 "$RC"
assert_contains "selector 2: an rc never satisfies a channel" "$OUT" "release candidates never satisfy a channel"

new_case
MINION_VERSION=v2.0.0-rc.1 run_boot x
assert_eq "exact release candidate is allowed" 0 "$RC"
assert_contains "exact release candidate: runs it" "$OUT" "fake-minion v2.0.0-rc.1 amd64"

new_case
MINION_VERSION='' MINION_SHA256='' run_boot x
assert_eq "empty variables count as unset" 0 "$RC"
assert_contains "empty MINION_VERSION means latest" "$OUT" "fake-minion v1.4.10"

for bad_sel in "../x" "1.2.3.4" "v01.2.3" "1.4.x" "latest;id" "-1" "1.4.2-beta"; do
	new_case
	MINION_VERSION="$bad_sel" run_boot x
	assert_eq "invalid selector '$bad_sel': refused" 1 "$RC"
	assert_contains "invalid selector '$bad_sel': message" "$OUT" "invalid MINION_VERSION"
	assert_eq "invalid selector '$bad_sel': nothing cached" 0 "$(entry_count)"
done

# ============================================ verification failures are fatal
new_case
FAKE_COSIGN_FAIL=1 MINION_VERSION=latest run_boot x
assert_eq "cosign failure: refused" 1 "$RC"
assert_contains "cosign failure: explains" "$OUT" "could not be verified"
assert_not_contains "cosign failure: minion is not run" "$OUT" "fake-minion"
assert_eq "cosign failure: nothing cached" 0 "$(entry_count)"

new_case
MINION_VERSION=v1.3.0 run_boot x
FAKE_COSIGN_FAIL=1 MINION_VERSION=1 run_boot x
assert_eq "cosign failure does not fall back to a cached older version" 1 "$RC"
assert_not_contains "cosign failure: the cached v1.3.0 is not run" "$OUT" "fake-minion"

# checksums.txt that disagrees with the archive (cosign passes, as for a valid
# signature over a bad file)
cp "$REL/v1.4.2/checksums.txt" "$WORK/checksums-1.4.2.orig"
sed 's/^[0-9a-f]\{64\}/0000000000000000000000000000000000000000000000000000000000000000/' "$WORK/checksums-1.4.2.orig" >"$REL/v1.4.2/checksums.txt"
new_case
MINION_VERSION=v1.4.2 run_boot x
assert_eq "archive differs from the signed checksums.txt: refused" 1 "$RC"
assert_contains "archive differs from checksums.txt: explains" "$OUT" "checksum mismatch"
assert_eq "archive differs from checksums.txt: nothing cached" 0 "$(entry_count)"
grep -v 'linux_amd64' "$WORK/checksums-1.4.2.orig" >"$REL/v1.4.2/checksums.txt"
new_case
MINION_VERSION=v1.4.2 run_boot x
assert_eq "archive missing from checksums.txt: refused" 1 "$RC"
assert_contains "archive missing from checksums.txt: explains" "$OUT" "does not list"
cp "$WORK/checksums-1.4.2.orig" "$REL/v1.4.2/checksums.txt"

# a cosign binary with the wrong digest is never executed
new_case
echo '#!/bin/sh
echo pwned >&2' >"$COSIGN_DIR/cosign-linux-amd64"
chmod 755 "$COSIGN_DIR/cosign-linux-amd64"
MINION_VERSION=latest run_boot x
assert_eq "cosign with a wrong sha256: refused" 1 "$RC"
assert_contains "cosign with a wrong sha256: explains" "$OUT" "the pinned value is"
assert_not_contains "cosign with a wrong sha256: not executed" "$OUT" "pwned"
cp "$COSIGN_DIR/cosign-fake" "$COSIGN_DIR/cosign-linux-amd64"

# ===================================================== AC 2: cache and offline
new_case
MINION_VERSION=1 run_boot x
first=$(entries)
stop_server
MINION_VERSION=latest run_boot again
assert_eq "offline: newest cached version matching the selector runs" 0 "$RC"
assert_contains "offline: runs the cached version" "$OUT" "fake-minion v1.4.10 amd64 args:again"
assert_contains "offline: says so" "$OUT" "cannot reach the release host"
assert_eq "offline: cache unchanged (entries are immutable)" "$first" "$(entries)"
MINION_VERSION=1.3 run_boot x
assert_eq "offline, nothing cached for the selector: fails" 1 "$RC"
assert_contains "offline, nothing cached: clear message" "$OUT" "no verified version matching it is cached"
MINION_VERSION=v1.4.10 run_boot exact
assert_eq "offline: exact version that is cached runs without network" 0 "$RC"
MINION_VERSION=v1.4.2 run_boot exact
assert_eq "offline: exact version that is not cached fails" 1 "$RC"
assert_contains "offline, exact not cached: clear message" "$OUT" "not cached"

# exact version + MINION_SHA256 on a later start without network
start_server
new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot first
assert_eq "exact + checksum, first start online: exit 0" 0 "$RC"
stop_server
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot second
assert_eq "exact + checksum, later start offline: runs from the cache" 0 "$RC"
assert_contains "exact + checksum offline: runs minion with the arguments" "$OUT" "fake-minion v1.4.2 amd64 args:second"
assert_not_contains "exact + checksum offline: nothing is downloaded" "$OUT" "downloading"
MINION_VERSION=v1.4.2 MINION_SHA256="1111111111111111111111111111111111111111111111111111111111111111" run_boot third
assert_eq "exact + a different checksum offline: refused" 1 "$RC"
assert_contains "exact + a different checksum offline: clear 'not cached' message" "$OUT" "not cached"
assert_not_contains "exact + a different checksum offline: the cached binary is not run" "$OUT" "fake-minion"
start_server

# a cache is bound to its architecture
new_case
FAKE_UNAME_M=x86_64 MINION_VERSION=v1.4.2 run_boot x
assert_contains "arch cache: amd64 start caches the amd64 binary" "$OUT" "fake-minion v1.4.2 amd64"
stop_server
FAKE_UNAME_M=aarch64 MINION_VERSION=v1.4.2 run_boot x
assert_eq "arch cache: an arm64 start offline never runs the cached amd64 binary" 1 "$RC"
assert_not_contains "arch cache: amd64 binary not run on arm64 (exact)" "$OUT" "fake-minion"
FAKE_UNAME_M=aarch64 MINION_VERSION=1.4 run_boot x
assert_eq "arch cache: an arm64 channel start offline never runs the amd64 binary" 1 "$RC"
assert_not_contains "arch cache: amd64 binary not run on arm64 (channel)" "$OUT" "fake-minion"
start_server
FAKE_UNAME_M=aarch64 MINION_VERSION=v1.4.2 run_boot x
assert_contains "arch cache: an arm64 start online installs its own binary" "$OUT" "fake-minion v1.4.2 arm64"
assert_eq "arch cache: both architectures coexist in one cache" 2 "$(entry_count)"
stop_server
FAKE_UNAME_M=aarch64 MINION_VERSION=v1.4.2 run_boot x
assert_contains "arch cache: arm64 offline runs the arm64 entry" "$OUT" "fake-minion v1.4.2 arm64"
FAKE_UNAME_M=x86_64 MINION_VERSION=v1.4.2 run_boot x
assert_contains "arch cache: amd64 offline runs the amd64 entry" "$OUT" "fake-minion v1.4.2 amd64"
start_server

# concurrent starts must never delete or break a published entry
RACEBIN="$WORK/racebin"
mkdir -p "$RACEBIN"
REAL_MV=$(command -v mv)
cat >"$RACEBIN/mv" <<MVW
#!/bin/sh
# Publishes a complete copy of the stage just before the real rename, as a
# competing start that finished first would.
if [ -f "\$1/.complete" ] && [ ! -e "\$2" ]; then
	cp -R "\$1" "\$2"
	echo raced >"\$2/raced"
fi
exec "$REAL_MV" "\$@"
MVW
chmod 755 "$RACEBIN/mv"
new_case
BOOT_PATH="$RACEBIN:$FAKEBIN:$PATH" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
BOOT_PATH="$FAKEBIN:$PATH"
assert_eq "lost publish race: the loser still runs minion" 0 "$RC"
assert_contains "lost publish race: runs the entry" "$OUT" "fake-minion v1.4.2 amd64 args:x"
assert_file "lost publish race: the competing entry survives (never deleted)" "$CACHE/v1.4.2-$SHA_142_AMD64/raced"
assert_eq "lost publish race: no stray stage directory inside the entry" "" "$(find "$CACHE/v1.4.2-$SHA_142_AMD64" -name '.tmp.*')"
assert_eq "lost publish race: one entry" 1 "$(entry_count)"

new_case
pids=""
for i in 1 2 3 4 5 6; do
	(MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot "p$i"; printf '%s %s\n' "$RC" "$(printf '%s' "$OUT" | grep -c "fake-minion v1.4.2 amd64 args:p$i")" >"$WORK/par$CASE_N.$i"; printf '%s\n' "$OUT" >"$WORK/par$CASE_N.$i.out") &
	pids="$pids $!"
done
# shellcheck disable=SC2086
wait $pids
par=$(cat "$WORK"/par"$CASE_N".?)
assert_eq "concurrent starts: all six ran minion successfully" "0 1
0 1
0 1
0 1
0 1
0 1" "$par"
assert_eq "concurrent starts: exactly one entry" 1 "$(entry_count)"
assert_eq "concurrent starts: no stray stage directory" "" "$(find "$CACHE/v1.4.2-$SHA_142_AMD64" -name '.tmp.*')"

# newest cached of several; a cached rc never satisfies a channel
start_server
new_case
MINION_VERSION=v1.3.0 run_boot x
MINION_VERSION=v1.4.1 run_boot x
MINION_VERSION=v1.4.2 run_boot x
MINION_VERSION=v2.0.0-rc.1 run_boot x
assert_eq "four cached entries" 4 "$(entry_count)"
stop_server
for pair in "latest:v1.4.2" "1:v1.4.2" "1.4:v1.4.2" "1.3:v1.3.0"; do
	sel=${pair%%:*}
	want=${pair#*:}
	MINION_VERSION="$sel" run_boot x
	assert_contains "offline selector '$sel' picks the newest cached match $want" "$OUT" "fake-minion $want amd64"
done
MINION_VERSION=2 run_boot x
assert_eq "offline: a cached rc does not satisfy channel 2" 1 "$RC"
MINION_VERSION=v2.0.0-rc.1 run_boot x
assert_eq "offline: the cached rc runs by its exact version" 0 "$RC"

# the channel resolves to a release whose download fails: use the cache, say so
start_server
new_case
MINION_VERSION=v1.3.0 run_boot x
mv "$REL/v1.4.10" "$WORK/v1.4.10.away"
MINION_VERSION=1 run_boot x
mv "$WORK/v1.4.10.away" "$REL/v1.4.10"
assert_eq "channel resolved but download fails: falls back to the cache" 0 "$RC"
assert_contains "download failure fallback: runs the cached version" "$OUT" "fake-minion v1.3.0 amd64"
assert_contains "download failure fallback: says so" "$OUT" "using the cached"

new_case
mv "$REL/v1.4.10" "$WORK/v1.4.10.away"
MINION_VERSION=1 run_boot x
mv "$WORK/v1.4.10.away" "$REL/v1.4.10"
assert_eq "channel resolved, download fails, empty cache: fails" 1 "$RC"
assert_contains "download failure, empty cache: clear message" "$OUT" "could not be downloaded"

# second start with the network: no re-download of an existing entry
new_case
MINION_VERSION=latest run_boot x
before=$(stat_of "$(entry_dir v1.4.10)/minion_1.4.10_linux_amd64.tar.gz")
MINION_VERSION=latest run_boot y
after=$(stat_of "$(entry_dir v1.4.10)/minion_1.4.10_linux_amd64.tar.gz")
assert_eq "a second start reuses the immutable entry" "$before" "$after"
assert_contains "a second start still runs it" "$OUT" "fake-minion v1.4.10 amd64 args:y"
assert_eq "a second start downloads cosign only once; install verifies live and offline, the second start proves the entry offline" 3 "$(grep -c verify-blob "$COSIGN_LOG")"
assert_eq "a second start fetches the pinned cosign only once" 1 "$(grep -c 'TUF_ROOT=' "$COSIGN_LOG" | awk '{print ($1 >= 1) ? 1 : 0}')"

# tampered cache entry
new_case
MINION_VERSION=latest run_boot x
echo 'echo tampered' >>"$(entry_dir v1.4.10)/minion_1.4.10_linux_amd64.tar.gz"
MINION_VERSION=latest run_boot x
assert_eq "tampered cached archive: refused" 1 "$RC"
assert_contains "tampered cached archive: explains" "$OUT" "does not hash to"

# a half-written entry (no .complete marker) is not used
new_case
mkdir -p "$CACHE/v1.4.10-deadbeef"
printf '#!/bin/sh\necho partial\n' >"$CACHE/v1.4.10-deadbeef/minion"
chmod 755 "$CACHE/v1.4.10-deadbeef/minion"
stop_server
MINION_VERSION=latest run_boot x
assert_eq "an incomplete cache entry is ignored" 1 "$RC"
assert_not_contains "an incomplete cache entry is not run" "$OUT" "partial"
start_server

# ================================== artifact trust: forged and tampered caches
# (finding: a cache entry is not trusted because its directory name or a
# checksum file next to the binary says so.) Every cached entry is proven again
# from its archive on every start.
FORGED_PIN="2222222222222222222222222222222222222222222222222222222222222222"
forge_entry() { # DIR: the exact forgery of the finding, an arbitrary binary with matching local files
	mkdir -p "$1"
	printf '#!/bin/sh\necho FORGED-RAN\n' >"$1/minion"
	chmod 755 "$1/minion"
	sha "$1/minion" >"$1/minion.sha256"
	printf '%s\n' amd64 >"$1/arch"
	: >"$1/.complete"
}

start_server
new_case
forge_entry "$CACHE/v1.4.2-$FORGED_PIN"
stop_server
MINION_VERSION=v1.4.2 MINION_SHA256="$FORGED_PIN" run_boot x
assert_eq "forged cache (name + local sha256 + arch + .complete) under a configured pin, offline: refused" 1 "$RC"
assert_not_contains "forged cache offline: the planted binary is never run" "$OUT" "FORGED-RAN"
start_server
new_case
forge_entry "$CACHE/v1.4.2-$FORGED_PIN"
MINION_VERSION=v1.4.2 MINION_SHA256="$FORGED_PIN" run_boot x
assert_eq "forged cache under a configured pin, online: refused (the download is not the pin)" 1 "$RC"
assert_not_contains "forged cache online: the planted binary is never run" "$OUT" "FORGED-RAN"
assert_contains "forged cache online: the pin check says so" "$OUT" "checksum mismatch"

# a forged legacy entry named for the real archive digest is still not run
new_case
forge_entry "$CACHE/v1.4.2-$SHA_142_AMD64"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "forged legacy entry named for the real pin: the real archive is installed and run" 0 "$RC"
assert_not_contains "forged legacy entry: the planted binary is not run" "$OUT" "FORGED-RAN"
assert_contains "forged legacy entry: the genuine bytes run" "$OUT" "fake-minion v1.4.2 amd64"
assert_no_file "forged legacy entry: the planted binary is gone from the entry" "$CACHE/v1.4.2-$SHA_142_AMD64/minion.sha256"

# a planted binary next to a genuine archive is ignored: the executed bytes come from the archive
new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot first
printf '#!/bin/sh\necho FORGED-RAN\n' >"$CACHE/v1.4.2-$SHA_142_AMD64/minion"
chmod 755 "$CACHE/v1.4.2-$SHA_142_AMD64/minion"
sha "$CACHE/v1.4.2-$SHA_142_AMD64/minion" >"$CACHE/v1.4.2-$SHA_142_AMD64/minion.sha256"
stop_server
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot second
assert_eq "planted binary + recorded digest beside a genuine archive: runs offline" 0 "$RC"
assert_not_contains "planted binary beside the archive: not run" "$OUT" "FORGED-RAN"
assert_contains "planted binary beside the archive: the bytes of the archive run" "$OUT" "fake-minion v1.4.2 amd64 args:second"

# tampered archive in the cache (the pin and the entry name no longer match)
echo tamper >>"$CACHE/v1.4.2-$SHA_142_AMD64/minion_1.4.2_linux_amd64.tar.gz"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot third
assert_eq "tampered cached archive: refused" 1 "$RC"
assert_contains "tampered cached archive: explains" "$OUT" "does not hash to"
assert_not_contains "tampered cached archive: nothing runs" "$OUT" "fake-minion"
MINION_VERSION=v1.4.2 MINION_SHA256="$FORGED_PIN" run_boot fourth
assert_eq "cached entry presented with a different pin: refused" 1 "$RC"
assert_not_contains "different pin: nothing runs" "$OUT" "fake-minion"
start_server

# an archive swapped in the cache between the proof and the exec does not change what runs
new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot first
SWAPBIN="$WORK/swapbin"
mkdir -p "$SWAPBIN"
REAL_SHA256SUM=$(command -v sha256sum)
EVIL="$WORK/evil"
mkdir -p "$EVIL"
printf '#!/bin/sh\necho FORGED-RAN\n' >"$EVIL/minion"
chmod 755 "$EVIL/minion"
tar -czf "$WORK/evil.tgz" -C "$EVIL" minion
cat >"$SWAPBIN/sha256sum" <<SWAP
#!/bin/sh
# After the digest of the staged copy is taken, replace the archive in the cache.
"$REAL_SHA256SUM" "\$@"
case "\$1" in
*/auth/*.tar.gz) cp "$WORK/evil.tgz" "$CACHE/v1.4.2-$SHA_142_AMD64/minion_1.4.2_linux_amd64.tar.gz" ;;
esac
SWAP
chmod 755 "$SWAPBIN/sha256sum"
BOOT_PATH="$SWAPBIN:$FAKEBIN:$PATH" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot second
BOOT_PATH="$FAKEBIN:$PATH"
assert_eq "archive swapped after the proof: still exit 0" 0 "$RC"
assert_not_contains "archive swapped after the proof: the swapped bytes never run" "$OUT" "FORGED-RAN"
assert_contains "archive swapped after the proof: the proven bytes run" "$OUT" "fake-minion v1.4.2 amd64 args:second"

# an unpinned (signature) cache entry is proven again, offline, with the signed checksums
new_case
MINION_VERSION=v1.4.10 run_boot first
e=$(entry_dir v1.4.10)
assert_file "signed entry keeps the archive" "$e/minion_1.4.10_linux_amd64.tar.gz"
assert_file "signed entry keeps the signed checksums.txt" "$e/checksums.txt"
assert_file "signed entry keeps the cosign bundle" "$e/checksums.txt.sigstore.json"
assert_file "signed entry keeps the trusted root it was verified with" "$e/trusted_root.json"
assert_no_file "no executable is stored in the entry" "$e/minion"
: >"$COSIGN_LOG"
stop_server
MINION_VERSION=v1.4.10 run_boot again
assert_eq "signed entry, offline exact start: exit 0" 0 "$RC"
assert_contains "signed entry, offline: runs" "$OUT" "fake-minion v1.4.10 amd64 args:again"
assert_contains "signed entry, offline: cosign verified the cached bundle again" "$(cat "$COSIGN_LOG")" "verify-blob --trusted-root"
assert_contains "signed entry, offline: with the stored trusted root, no TUF refresh" "$(cat "$COSIGN_LOG")" "$CACHE/.tmp."
FAKE_COSIGN_FAIL=1 MINION_VERSION=v1.4.10 run_boot again
assert_eq "signed entry: a failing signature check refuses the cached entry" 1 "$RC"
assert_not_contains "signed entry, failing signature: nothing runs" "$OUT" "fake-minion"
# a forged proof: another archive with a checksums.txt that lists it (the signature, which cosign
# here accepts, covers the original checksums.txt only; the real cosign rejects the bundle)
cp "$e/checksums.txt" "$WORK/cs.orig"
sed 's/^[0-9a-f]\{64\}/1111111111111111111111111111111111111111111111111111111111111111/' "$WORK/cs.orig" >"$e/checksums.txt"
MINION_VERSION=v1.4.10 run_boot again
assert_eq "signed entry: checksums.txt that does not list the archive digest is refused" 1 "$RC"
assert_not_contains "signed entry, tampered checksums.txt: nothing runs" "$OUT" "fake-minion"
cp "$WORK/cs.orig" "$e/checksums.txt"
# a signed entry without its proof files is not used offline
rm -f "$e/trusted_root.json"
MINION_VERSION=v1.4.10 run_boot again
assert_eq "signed entry without its trusted root: not used offline" 1 "$RC"
assert_not_contains "signed entry without its trusted root: nothing runs" "$OUT" "fake-minion"
start_server

# an entry made under a pin is not trusted by an unpinned start
new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot first
stop_server
MINION_VERSION=v1.4.2 run_boot second
assert_eq "pin-only entry, unpinned offline start: refused" 1 "$RC"
assert_not_contains "pin-only entry, unpinned start: nothing runs" "$OUT" "fake-minion"
MINION_VERSION=latest run_boot third
assert_eq "pin-only entry, channel offline start: refused" 1 "$RC"
assert_not_contains "pin-only entry, channel start: nothing runs" "$OUT" "fake-minion"
start_server

# ---- the cache boundary: ownership, modes, links and ancestry
# A fake ls reports another owner (FAKE_OWNER_PATH) or another group
# (FAKE_GROUP_PATH) for one path: a lower-privilege writer, whatever user runs
# the test.
OWNBIN="$WORK/ownbin"
mkdir -p "$OWNBIN"
REAL_LS=$(command -v ls)
cat >"$OWNBIN/ls" <<OWN
#!/bin/sh
out=\$("$REAL_LS" "\$@") || exit \$?
if [ -n "\${FAKE_OWNER_PATH:-}" ]; then
	case "\$out" in *" \$FAKE_OWNER_PATH" | *" \$FAKE_OWNER_PATH -> "*) printf '%s\n' "\$out" | awk '{ \$3 = "4242"; print }'; exit 0 ;; esac
fi
if [ -n "\${FAKE_GROUP_PATH:-}" ]; then
	case "\$out" in *" \$FAKE_GROUP_PATH" | *" \$FAKE_GROUP_PATH -> "*) printf '%s\n' "\$out" | awk '{ \$4 = "4343"; print }'; exit 0 ;; esac
fi
printf '%s\n' "\$out"
OWN
chmod 755 "$OWNBIN/ls"

new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot first
E="$CACHE/v1.4.2-$SHA_142_AMD64"

BOOT_PATH="$OWNBIN:$FAKEBIN:$PATH" FAKE_OWNER_PATH="$CACHE" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache directory owned by another user: refused" 1 "$RC"
assert_contains "cache owned by another user: says what is wrong" "$OUT" "is owned by uid 4242"
assert_contains "cache owned by another user: gives the operator guidance" "$OUT" "will not change it"
assert_not_contains "cache owned by another user: nothing runs" "$OUT" "fake-minion"

BOOT_PATH="$OWNBIN:$FAKEBIN:$PATH" FAKE_OWNER_PATH="$E" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache entry owned by another user: refused" 1 "$RC"
assert_contains "cache entry owned by another user: says what is wrong" "$OUT" "is owned by uid 4242"
assert_not_contains "cache entry owned by another user: nothing runs" "$OUT" "fake-minion"

BOOT_PATH="$OWNBIN:$FAKEBIN:$PATH" FAKE_OWNER_PATH="$E/minion_1.4.2_linux_amd64.tar.gz" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cached archive owned by another user: refused" 1 "$RC"
assert_not_contains "cached archive owned by another user: nothing runs" "$OUT" "fake-minion"

BOOT_PATH="$OWNBIN:$FAKEBIN:$PATH" FAKE_OWNER_PATH="$WORK" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "ancestor of the cache owned by another user: refused" 1 "$RC"
assert_contains "ancestor owned by another user: names it" "$OUT" "ancestor"
assert_not_contains "ancestor owned by another user: nothing runs" "$OUT" "fake-minion"
BOOT_PATH="$FAKEBIN:$PATH"

chmod 1777 "$CACHE"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
chmod 755 "$CACHE"
assert_eq "sticky world-writable cache directory itself (MINION_CACHE_DIR=/tmp): refused" 1 "$RC"
assert_contains "sticky world-writable cache directory: says what is wrong" "$OUT" "writable by group or others"
assert_not_contains "sticky world-writable cache directory: nothing runs" "$OUT" "fake-minion"

chmod 777 "$CACHE"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
chmod 755 "$CACHE"
assert_eq "world-writable cache directory: refused" 1 "$RC"
assert_contains "world-writable cache: says what is wrong" "$OUT" "writable by group or others"
assert_not_contains "world-writable cache: nothing runs" "$OUT" "fake-minion"

chmod 757 "$E"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
chmod 755 "$E"
assert_eq "cache entry writable by others: refused" 1 "$RC"
assert_not_contains "cache entry writable by others: nothing runs" "$OUT" "fake-minion"

# group write: a group (even the user's own primary group or root's) can have other members,
# so group-writable is refused like world-writable; there is no list of trusted writers
chmod 775 "$E"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache entry writable by the user's own group: refused" 1 "$RC"
assert_contains "own group: says what is wrong" "$OUT" "writable by group or others"
assert_not_contains "cache entry writable by the user's own group: nothing runs" "$OUT" "fake-minion"
chmod 755 "$E"
chmod 775 "$CACHE"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
chmod 755 "$CACHE"
assert_eq "cache directory writable by the user's own group: refused" 1 "$RC"
assert_not_contains "cache directory writable by own group: nothing runs" "$OUT" "fake-minion"
mkdir -p "$CACHE/.run"
chmod 775 "$CACHE/.run"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
chmod 700 "$CACHE/.run"
assert_eq "run directory writable by the group (a member could replace the executable after the proof): refused" 1 "$RC"
assert_not_contains "group-writable run directory: nothing runs" "$OUT" "fake-minion"
chmod 775 "$E"
BOOT_PATH="$OWNBIN:$FAKEBIN:$PATH" FAKE_GROUP_PATH="$E" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
BOOT_PATH="$FAKEBIN:$PATH"
chmod 755 "$E"
assert_eq "cache entry writable by a foreign group: refused" 1 "$RC"
assert_contains "foreign group: names the writers" "$OUT" "writable by group or others"
assert_not_contains "cache entry writable by a foreign group: nothing runs" "$OUT" "fake-minion"

chmod 666 "$E/minion_1.4.2_linux_amd64.tar.gz"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
chmod 644 "$E/minion_1.4.2_linux_amd64.tar.gz"
assert_eq "world-writable cached archive: refused" 1 "$RC"
assert_not_contains "world-writable cached archive: nothing runs" "$OUT" "fake-minion"

mv "$E/minion_1.4.2_linux_amd64.tar.gz" "$WORK/real.tgz"
ln -s "$WORK/real.tgz" "$E/minion_1.4.2_linux_amd64.tar.gz"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
rm -f "$E/minion_1.4.2_linux_amd64.tar.gz"
mv "$WORK/real.tgz" "$E/minion_1.4.2_linux_amd64.tar.gz"
assert_eq "symlinked cached archive: refused" 1 "$RC"
assert_not_contains "symlinked cached archive: nothing runs" "$OUT" "fake-minion"

mv "$E" "$WORK/real-entry"
ln -s "$WORK/real-entry" "$E"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
rm -f "$E"
mv "$WORK/real-entry" "$E"
assert_eq "symlinked cache entry: refused" 1 "$RC"
assert_not_contains "symlinked cache entry: nothing runs" "$OUT" "fake-minion"

MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "after restoring the modes the entry runs again" 0 "$RC"

# an ancestor of the cache that anyone can write is refused unless it is sticky like /tmp
mkdir -p "$WORK/open"
chmod 777 "$WORK/open"
OPENCACHE="$WORK/open/cache"
mkdir -p "$OPENCACHE"
CACHE_SAVE=$CACHE
CACHE=$OPENCACHE
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache below a world-writable directory: refused" 1 "$RC"
assert_contains "world-writable ancestor: names it" "$OUT" "ancestor"
chmod 1777 "$WORK/open"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache below a sticky world-writable directory (like /tmp): allowed" 0 "$RC"
CACHE=$CACHE_SAVE

# a symlinked cache root that points into a trusted directory works; one owned by another user is refused
mkdir -p "$WORK/real-cache"
ln -s "$WORK/real-cache" "$WORK/link-cache"
CACHE_SAVE=$CACHE
CACHE="$WORK/link-cache"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache reached through an own symlink: allowed" 0 "$RC"
BOOT_PATH="$OWNBIN:$FAKEBIN:$PATH" FAKE_OWNER_PATH="$WORK/link-cache" MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
BOOT_PATH="$FAKEBIN:$PATH"
assert_eq "cache reached through a symlink owned by another user: refused" 1 "$RC"
CACHE=$CACHE_SAVE

# an intermediate symlink hop is checked like any other directory on the way:
# safe/hub -> shared/hop -> safe/data, with shared writable by anyone, lets another
# user replace the hop and redirect the cache, although the start and the end are safe
mkdir -p "$WORK/hop/safe/data" "$WORK/hop/shared"
chmod 755 "$WORK/hop/safe" "$WORK/hop/safe/data"
chmod 777 "$WORK/hop/shared"
ln -s "$WORK/hop/safe/data" "$WORK/hop/shared/hop"
ln -s "$WORK/hop/shared/hop" "$WORK/hop/safe/hub"
CACHE_SAVE=$CACHE
CACHE="$WORK/hop/safe/hub/cache"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache behind a hop that lives in a world-writable directory: refused" 1 "$RC"
assert_contains "intermediate hop: names the unsafe directory" "$OUT" "$WORK/hop/shared"
assert_not_contains "intermediate hop: nothing runs" "$OUT" "fake-minion"
chmod 755 "$WORK/hop/shared"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "the same chain with every hop in a trusted directory: allowed" 0 "$RC"
# a relative link and a link cycle
rm -f "$WORK/hop/shared/hop" "$WORK/hop/safe/hub"
ln -s data "$WORK/hop/safe/rel"
CACHE="$WORK/hop/safe/rel/cache2"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "cache behind a relative symlink in a trusted directory: allowed" 0 "$RC"
ln -s "$WORK/hop/safe/loop2" "$WORK/hop/safe/loop1"
ln -s "$WORK/hop/safe/loop1" "$WORK/hop/safe/loop2"
CACHE="$WORK/hop/safe/loop1/cache"
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot x
assert_eq "a symlink cycle in the cache path: refused" 1 "$RC"
CACHE=$CACHE_SAVE

# ---- publication: the check for a missing or incomplete entry and the move of debris
# are one critical section (a lock directory), so a starter that is not yet at the rename
# cannot displace an entry another starter completed, and a pinned-only starter cannot
# replace a signed proof with weaker metadata.
D_1410=$(sha "$REL/v1.4.10/minion_1.4.10_linux_amd64.tar.gz")
new_case
MINION_VERSION=v1.4.10 run_boot x
rm -rf "$WORK/signed-copy"
cp -R "$(entry_dir v1.4.10)" "$WORK/signed-copy"
echo B-was-here >"$WORK/signed-copy/raced"

# wait_for_stage: the background start has downloaded its archive and is past the download
wait_for_stage() { # CACHE
	n=0
	while [ "$n" -lt 100 ]; do
		for f in "$1"/.tmp.*/work/*.tar.gz; do [ -f "$f" ] && return 0; done
		n=$((n + 1))
		sleep 0.1 2>/dev/null || sleep 1
	done
	return 1
}

race_case() { # NAME PREPARE: a pinned-only start A is held at the lock while B publishes a signed entry
	new_case
	mkdir -p "$CACHE"
	chmod 755 "$CACHE"
	[ -z "$2" ] || "$2" "$CACHE/v1.4.10-$D_1410"
	mkdir "$CACHE/.lock.v1.4.10-$D_1410" # B holds the lock
	rm -f "$WORK/a.rc" "$WORK/a.out"
	(
		MINION_LOCK_WAIT=30 MINION_VERSION=v1.4.10 MINION_SHA256="$D_1410" run_boot a
		printf '%s' "$RC" >"$WORK/a.rc"
		printf '%s\n' "$OUT" >"$WORK/a.out"
	) &
	apid=$!
	if wait_for_stage "$CACHE"; then ok "$1: A reached the publication step"; else bad "$1: A reached the publication step"; fi
	sleep 1 2>/dev/null
	if [ -e "$WORK/a.rc" ]; then bad "$1: A must wait while the lock is held" "$(cat "$WORK/a.out")"; else ok "$1: A waits while the lock is held"; fi
	# B publishes (its complete signed entry replaces the debris) and releases the lock
	rm -rf "$CACHE/v1.4.10-$D_1410"
	cp -R "$WORK/signed-copy" "$CACHE/v1.4.10-$D_1410"
	rmdir "$CACHE/.lock.v1.4.10-$D_1410"
	wait "$apid"
	AOUT=$(cat "$WORK/a.out" 2>/dev/null)
	assert_eq "$1: A finished and ran minion" "0" "$(cat "$WORK/a.rc" 2>/dev/null)"
	assert_contains "$1: A ran the verified bytes" "$AOUT" "fake-minion v1.4.10 amd64 args:a"
	E_=$CACHE/v1.4.10-$D_1410
	assert_file "$1: B's entry was not displaced" "$E_/raced"
	assert_file "$1: the signed proof is kept (bundle)" "$E_/checksums.txt.sigstore.json"
	assert_file "$1: the signed proof is kept (trusted root)" "$E_/trusted_root.json"
	assert_eq "$1: the lock is released and not left behind" "" "$(find "$CACHE" -maxdepth 1 -name '.lock.*')"
	assert_eq "$1: no debris of the competing entry in the cache" "" "$(find "$CACHE" -maxdepth 1 -name 'debris*')"
}
prep_absent() { :; }
prep_incomplete() { mkdir -p "$1" && printf 'x' >"$1/partial"; } # debris: no archive, no .complete
prep_legacy() { forge_entry "$1"; }                           # an entry of an earlier script version
race_case "absent entry" ""
race_case "incomplete entry (debris rename)" prep_incomplete
race_case "legacy entry (debris rename)" prep_legacy

# a lock that is never released: bounded wait, fail closed, nothing deleted, nothing stolen
new_case
mkdir -p "$CACHE"
chmod 755 "$CACHE"
mkdir "$CACHE/.lock.v1.4.10-$D_1410"
mkdir -p "$CACHE/v1.4.10-$D_1410"
printf 'x' >"$CACHE/v1.4.10-$D_1410/partial"
MINION_LOCK_WAIT=2 MINION_VERSION=v1.4.10 MINION_SHA256="$D_1410" run_boot a
assert_eq "a lock that is never released: the start fails closed" 1 "$RC"
assert_contains "stuck lock: tells the operator what to check" "$OUT" ".lock.v1.4.10-$D_1410"
assert_not_contains "stuck lock: minion is not run" "$OUT" "fake-minion"
assert_dir_exists "stuck lock: the lock of the other start is not stolen" "$CACHE/.lock.v1.4.10-$D_1410"
assert_file "stuck lock: the uncertain entry is not deleted" "$CACHE/v1.4.10-$D_1410/partial"

# a normal start and a normal failure leave no lock behind
new_case
MINION_VERSION=v1.4.10 MINION_SHA256="$D_1410" run_boot a
assert_eq "no lock is left after a start" "" "$(find "$CACHE" -maxdepth 1 -name '.lock.*')"
new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$FORGED_PIN" run_boot a
assert_eq "no lock is left after a refused start" "" "$(find "$CACHE" -maxdepth 1 -name '.lock.*')"

# ---- the signature trust anchor: a fresh private TUF directory per verification, and the
# exact root that verified the release is the one stored, proven again offline
# A restored cache can hold a writable TUF directory with a custom remote and roots chosen by
# someone else; none of it may reach cosign.
new_case
mkdir -p "$CACHE/tools/sigstore-tuf/attacker-mirror/targets"
chmod 777 "$CACHE/tools/sigstore-tuf"
echo '{"mirror":"https://evil.invalid"}' >"$CACHE/tools/sigstore-tuf/remote.json"
echo evil-root >"$CACHE/tools/sigstore-tuf/root.json"
echo '{"fake":"attacker trusted root"}' >"$CACHE/tools/sigstore-tuf/attacker-mirror/targets/trusted_root.json"
MINION_VERSION=v1.4.10 run_boot x
assert_eq "a writable cached TUF directory with an attacker's remote and root: the install still succeeds" 0 "$RC"
log=$(cat "$COSIGN_LOG")
assert_not_contains "cached TUF directory: cosign never uses it" "$log" "$CACHE/tools/sigstore-tuf"
assert_contains "cached TUF directory: cosign starts from an empty TUF directory" "$log" "tufstart: "
assert_not_contains "cached TUF directory: nothing from the cache is imported" "$log" "tufstart: attacker"
assert_not_contains "cached TUF directory: no remote.json or root.json is imported" "$log" "remote.json"
e=$(entry_dir v1.4.10)
assert_contains "cached TUF directory: the stored root is the one of the live verification" "$(cat "$e/trusted_root.json")" "production trusted root"
assert_not_contains "cached TUF directory: the attacker's root is not stored" "$(cat "$e/trusted_root.json")" "attacker"
# shellcheck disable=SC2012
assert_eq "cached TUF directory: the stale directory is left alone (no broad cleanup)" "attacker-mirror remote.json root.json" "$(ls -A "$CACHE/tools/sigstore-tuf" | tr '\n' ' ' | sed 's/ $//')"

# the root has to be the one at the production mirror's path, the only one, and it has to verify offline
signed_state() { # NAME: after an install with a bad root the entry is not signed
	e=$(entry_dir v1.4.10)
	if [ -f "$e/trusted_root.json" ]; then bad "$1: the entry must not be marked signed"; else ok "$1: the entry is not marked signed"; fi
	assert_eq "$1: the install itself still ran minion (it was verified live)" 0 "$RC"
	assert_contains "$1: says why the entry is pin-only" "$OUT" "can be used offline only with MINION_SHA256"
	stop_server
	MINION_VERSION=v1.4.10 run_boot again
	assert_eq "$1: an unpinned offline start does not use the entry" 1 "$RC"
	assert_not_contains "$1: nothing runs unpinned offline" "$OUT" "fake-minion"
	MINION_VERSION=v1.4.10 MINION_SHA256="$D_1410" run_boot again
	assert_eq "$1: the explicit pin still authenticates the cached archive offline" 0 "$RC"
	start_server
}
new_case
FAKE_NO_ROOT=1 MINION_VERSION=v1.4.10 run_boot x
e=$(entry_dir v1.4.10)
assert_eq "no root from the live verification: nothing is stored" "" "$(cat "$e/trusted_root.json" 2>/dev/null)"
signed_state "no root captured"

new_case
mkdir -p "$CACHE/tools/sigstore-tuf/aaa-unrelated/targets"
echo '{"fake":"unrelated cached root"}' >"$CACHE/tools/sigstore-tuf/aaa-unrelated/targets/trusted_root.json"
FAKE_NO_ROOT=1 MINION_VERSION=v1.4.10 run_boot x
signed_state "an unrelated root in the cache and none from the live verification"

new_case
FAKE_ROOT_CONTENT="an unrelated root" MINION_VERSION=v1.4.10 run_boot x
signed_state "a root that does not verify the bundle offline"

new_case
FAKE_MIRROR=custom-mirror.example MINION_VERSION=v1.4.10 run_boot x
signed_state "a root of a mirror other than the production one"

new_case
FAKE_EXTRA_ROOT=1 MINION_VERSION=v1.4.10 run_boot x
signed_state "two roots in the TUF directory (no first match is chosen)"

# the good case again: signed, with the exact production root, verified offline before publishing
new_case
MINION_VERSION=v1.4.10 run_boot x
e=$(entry_dir v1.4.10)
assert_file "the exact root of the live verification is stored with the signed entry" "$e/trusted_root.json"
assert_eq "the stored root is byte-identical to the production root of the live verification" '{"fake":"production trusted root"}' "$(cat "$e/trusted_root.json")"
assert_eq "install: one live and one offline verification" 2 "$(grep -c verify-blob "$COSIGN_LOG")"
assert_contains "install: the offline re-verification uses the staged root" "$(cat "$COSIGN_LOG")" "verify-blob --trusted-root $CACHE/.tmp."

# ---- genuine pinned cosign (optional): MINION_TEST_REAL_COSIGN_DIR holds the real pinned cosign
# binary as "cosign" plus cosign_checksums.txt and cosign_checksums.txt.sigstore.json of the same
# release; the live part needs the network. This proves the TUF layout the script relies on
# and the offline --trusted-root mechanism with a genuine signature. It is NOT a Minion release
# test: no release signed with Minion's identity exists yet, so the Minion identity is only
# proven to refuse this foreign signature.
RC_DIR=${MINION_TEST_REAL_COSIGN_DIR:-}
if [ -n "$RC_DIR" ] && [ -x "$RC_DIR/cosign" ] && [ -s "$RC_DIR/cosign_checksums.txt.sigstore.json" ]; then
	RC_BIN="$RC_DIR/cosign"
	assert_eq "real cosign: the binary is the pinned one" "$(sed -n 's/^COSIGN_SHA256_AMD64="\(.*\)"/\1/p' "$SCRIPT")" "$(sha "$RC_BIN")"
	RC_TUF="$WORK/rc-tuf"
	mkdir "$RC_TUF"
	RC_ID="keyless@projectsigstore.iam.gserviceaccount.com"
	RC_ISS="https://accounts.google.com"
	if out=$(TUF_ROOT="$RC_TUF" "$RC_BIN" verify-blob --bundle "$RC_DIR/cosign_checksums.txt.sigstore.json" --certificate-identity "$RC_ID" --certificate-oidc-issuer "$RC_ISS" "$RC_DIR/cosign_checksums.txt" 2>&1); then
		ok "real cosign: a fresh private TUF directory verifies a genuine bundle live"
		n=$(find "$RC_TUF" -name trusted_root.json | grep -c .)
		assert_eq "real cosign: exactly one trusted_root.json in the fresh TUF directory" 1 "$n"
		assert_file "real cosign: it is at the production mirror's path the script reads" "$RC_TUF/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json"
		cp "$RC_TUF/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json" "$WORK/rc-root.json"
		# offline: no TUF content, a dead proxy, only the staged root
		if out=$(HTTPS_PROXY=http://127.0.0.1:1 HTTP_PROXY=http://127.0.0.1:1 TUF_ROOT="$WORK/rc-empty" "$RC_BIN" verify-blob --trusted-root "$WORK/rc-root.json" --bundle "$RC_DIR/cosign_checksums.txt.sigstore.json" --certificate-identity "$RC_ID" --certificate-oidc-issuer "$RC_ISS" "$RC_DIR/cosign_checksums.txt" 2>&1); then
			ok "real cosign: the stored root verifies the same bundle fully offline"
		else
			bad "real cosign: the stored root verifies the same bundle fully offline" "$out"
		fi
		# another root (an empty trust configuration) does not verify it
		echo '{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1"}' >"$WORK/rc-wrong.json"
		if HTTPS_PROXY=http://127.0.0.1:1 TUF_ROOT="$WORK/rc-empty" "$RC_BIN" verify-blob --trusted-root "$WORK/rc-wrong.json" --bundle "$RC_DIR/cosign_checksums.txt.sigstore.json" --certificate-identity "$RC_ID" --certificate-oidc-issuer "$RC_ISS" "$RC_DIR/cosign_checksums.txt" >/dev/null 2>&1; then
			bad "real cosign: a wrong root must not verify the bundle"
		else
			ok "real cosign: a wrong root does not verify the bundle"
		fi
	else
		bad "real cosign: a fresh private TUF directory verifies a genuine bundle live (network needed)" "$out"
	fi
	# the script itself, with the real binary and the pinned digest: offline cached entry of Minion's
	# identity constraints must refuse a foreign signature (negative; not a Minion release)
	RC_CACHE="$WORK/rc-cache"
	mkdir -p "$RC_CACHE/tools"
	cp "$RC_BIN" "$RC_CACHE/tools/cosign-v3.1.4-amd64"
	chmod 755 "$RC_CACHE" "$RC_CACHE/tools"
	RC_D=$(printf 'archive' | sha256sum | awk '{print $1}')
	RC_E="$RC_CACHE/v9.9.9-$RC_D"
	mkdir "$RC_E"
	printf 'archive' >"$RC_E/minion_9.9.9_linux_amd64.tar.gz"
	cp "$RC_DIR/cosign_checksums.txt" "$RC_E/checksums.txt"
	cp "$RC_DIR/cosign_checksums.txt.sigstore.json" "$RC_E/checksums.txt.sigstore.json"
	[ ! -f "$WORK/rc-root.json" ] || cp "$WORK/rc-root.json" "$RC_E/trusted_root.json"
	echo amd64 >"$RC_E/arch"
	: >"$RC_E/.complete"
	chmod -R go-w "$RC_CACHE"
	OUT=$(env PATH="$FAKEBIN:$PATH" HOME=/nonexistent MINION_CACHE_DIR="$RC_CACHE" MINION_VERSION=v9.9.9 HTTPS_PROXY=http://127.0.0.1:1 sh "$SCRIPT" version 2>&1)
	RC=$?
	assert_eq "real cosign + Minion's identity constraints: a foreign signature is refused offline" 1 "$RC"
	assert_contains "real cosign: refused at the certificate identity" "$OUT" "expected SAN value \"https://github.com/patrick-1984/ai-minion/.github/workflows/release.yml@refs/tags/v9.9.9\""
	assert_not_contains "real cosign: nothing runs" "$OUT" "fake-minion"
else
	echo "skip genuine pinned cosign tests (set MINION_TEST_REAL_COSIGN_DIR to run them; they need the network)"
fi

# the legitimate offline cache survives: pinned, signed, and per architecture
new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot first
MINION_VERSION=v1.4.10 run_boot first
stop_server
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot second
assert_eq "legitimate pinned cache offline: runs" 0 "$RC"
MINION_VERSION=1 run_boot second
assert_eq "legitimate signed cache offline, channel: runs" 0 "$RC"
assert_contains "legitimate signed cache offline: newest signed entry" "$OUT" "fake-minion v1.4.10 amd64 args:second"
assert_dir_exists "the run directory of the executed bytes is private to the cache owner" "$CACHE/.run"
start_server

# ====================================================== architecture detection
for pair in "x86_64:amd64" "aarch64:arm64" "arm64:arm64" "armv7l:armv7" "armv8l:armv7"; do
	m=${pair%%:*}
	want=${pair#*:}
	new_case
	FAKE_UNAME_M="$m" MINION_VERSION=v1.4.2 run_boot x
	assert_eq "arch $m: exit 0" 0 "$RC"
	assert_contains "arch $m runs the $want archive" "$OUT" "fake-minion v1.4.2 $want args:x"
done
new_case
FAKE_UNAME_M=armv6l MINION_VERSION=v1.4.2 run_boot x
assert_eq "armv6l: unsupported" 1 "$RC"
assert_contains "armv6l: names the supported architectures" "$OUT" "supported: amd64, arm64, armv7"
new_case
FAKE_UNAME_S=Darwin MINION_VERSION=v1.4.2 run_boot x
assert_eq "non-Linux: unsupported" 1 "$RC"
assert_contains "non-Linux: message" "$OUT" "unsupported operating system"

new_case
FAKE_UNAME_M=armv7l MINION_VERSION=latest run_boot x
assert_contains "armv7 + cosign uses the arm cosign asset" "$OUT" "fake-minion v1.4.10 armv7"
assert_file "armv7: cosign cached per arch" "$CACHE/tools/cosign-v3.1.4-armv7"

# ----------------------------------------------------------- script properties
new_case
MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" FAKE_MINION_EXIT=7 run_boot x
assert_eq "exec: the exit status of minion is the exit status of the script" 7 "$RC"

# the pins: every architecture has a 64-hex sha256 and the identity matches the
# release workflow
for var in COSIGN_SHA256_AMD64 COSIGN_SHA256_ARM64 COSIGN_SHA256_ARMV7; do
	assert_eq "pinned $var is 64 hex digits" 1 "$(grep -cE "^$var=\"[0-9a-f]{64}\"\$" "$SCRIPT")"
done
assert_eq "the pinned cosign version equals COSIGN_VERSION of release.yml" \
	"$(sed -n 's/^COSIGN_VERSION="\(.*\)"/\1/p' "$SCRIPT")" \
	"$(sed -n 's/^  COSIGN_VERSION: \(.*\)/\1/p' "$HERE/../.github/workflows/release.yml")"
assert_contains "the identity names the release workflow" "$(grep '^SIGNER_WORKFLOW=' "$SCRIPT")" "/.github/workflows/release.yml"
assert_contains "the identity names this repository" "$(grep '^REPO=' "$SCRIPT")" "patrick-1984/ai-minion"
assert_contains "the issuer is GitHub Actions" "$(grep '^OIDC_ISSUER=' "$SCRIPT")" "https://token.actions.githubusercontent.com"
assert_not_contains "no secret handling in argv: the script never reads a token" "$(cat "$SCRIPT")" "Authorization"

# the cross-check with Story 9.2: the updater and bootstrap verify the same signer
IDENTITY_GO="$HERE/../internal/version/identity.go"
if [ -f "$IDENTITY_GO" ]; then
	id_text=$(cat "$IDENTITY_GO")
	repo=$(sed -n 's/^REPO="\(.*\)"/\1/p' "$SCRIPT")
	# shellcheck disable=SC2016
	workflow_path=$(sed -n 's|^SIGNER_WORKFLOW="https://github.com/\${REPO}/\(.*\)"|\1|p' "$SCRIPT")
	assert_contains "identity.go (9.2): repository owner equals the script's" "$id_text" "\"${repo%%/*}\""
	assert_contains "identity.go (9.2): repository name equals the script's" "$id_text" "\"${repo#*/}\""
	assert_eq "script workflow path is the release workflow" ".github/workflows/release.yml" "$workflow_path"
	assert_contains "identity.go (9.2): workflow path equals the script's" "$id_text" "\"$workflow_path\""
	assert_contains "identity.go (9.2): OIDC issuer equals the script's" "$id_text" "\"$(sed -n 's/^OIDC_ISSUER="\(.*\)"/\1/p' "$SCRIPT")\""
else
	echo "skip identity.go cross-check (internal/version/identity.go of Story 9.2 is not in this tree yet)"
fi

# ====================================================== the wget downloader
# Same script, PATH limited to symlinks of the tools it needs (no curl) and a
# wget wrapper that logs its arguments.
if command -v wget >/dev/null 2>&1; then
	WGETBIN="$WORK/wgetbin"
	mkdir -p "$WGETBIN"
	for t in sh env wget tar gzip awk sed grep sha256sum mktemp tr sort head cat find id rm mkdir mv chmod cp ls kill sleep date dirname; do
		src=$(command -v "$t" 2>/dev/null) || continue
		case "$src" in /*) ;; *) continue ;; esac
		if [ "$t" = wget ]; then
			# shellcheck disable=SC2016
			printf '#!/bin/sh\necho "$*" >>"$WGET_LOG"\nexec %s "$@"\n' "$src" >"$WGETBIN/wget"
			chmod 755 "$WGETBIN/wget"
		else
			ln -s "$src" "$WGETBIN/$t"
		fi
	done
	cp "$FAKEBIN/uname" "$WGETBIN/uname"
	assert_eq "wget runs: curl is not on the limited PATH" "" "$(PATH="$WGETBIN" command -v curl 2>/dev/null || true)"
	BOOT_PATH="$WGETBIN"
	start_server

	new_case
	WGET_LOG="$WORK/wget$CASE_N.log"
	: >"$WGET_LOG"
	MINION_VERSION=v1.4.2 MINION_SHA256="$SHA_142_AMD64" run_boot hub serve
	assert_eq "wget: exact version + checksum: exit 0" 0 "$RC"
	assert_contains "wget: exact version + checksum: runs minion" "$OUT" "fake-minion v1.4.2 amd64 args:hub serve"
	assert_file "wget: exact version + checksum: immutable entry" "$CACHE/v1.4.2-$SHA_142_AMD64/minion_1.4.2_linux_amd64.tar.gz"
	assert_contains "wget: the retries are bounded (-t 2)" "$(cat "$WGET_LOG")" "-t 2"
	assert_contains "wget: the download went through wget" "$(cat "$WGET_LOG")" "/download/v1.4.2/minion_1.4.2_linux_amd64.tar.gz"

	new_case
	WGET_LOG="$WORK/wget$CASE_N.log"
	: >"$WGET_LOG"
	MINION_VERSION=latest run_boot hub serve
	assert_eq "wget: channel + cosign: exit 0" 0 "$RC"
	assert_contains "wget: channel resolves the newest stable release" "$OUT" "fake-minion v1.4.10 amd64 args:hub serve"
	assert_contains "wget: cosign verified the bundle" "$(cat "$COSIGN_LOG")" "--certificate-github-workflow-ref refs/tags/v1.4.10 "
	assert_contains "wget: the cosign binary and the release list went through wget" "$(cat "$WGET_LOG")" "/cosign/cosign-linux-amd64"
	first=$(entries)
	stop_server
	MINION_VERSION=latest run_boot again
	assert_eq "wget: offline start falls back to the cache" 0 "$RC"
	assert_contains "wget: offline runs the cached version" "$OUT" "fake-minion v1.4.10 amd64 args:again"
	assert_eq "wget: offline cache unchanged" "$first" "$(entries)"
	MINION_VERSION=1.3 run_boot x
	assert_eq "wget: offline with nothing cached fails" 1 "$RC"
	assert_contains "wget: offline failure message" "$OUT" "no verified version matching it is cached"

	BOOT_PATH="$FAKEBIN:$PATH"
	WGET_LOG=""
	start_server
else
	echo "skip wget downloader tests (wget is not installed)"
fi

# ------------------------------------------------------------ Compose and lint
COMPOSE="$HERE/compose/hub-bootstrap.yaml"
assert_file "compose example exists" "$COMPOSE"
compose_text=$(cat "$COMPOSE")
assert_contains "compose: runs bootstrap.sh with hub serve" "$compose_text" "bootstrap.sh"
assert_contains "compose: version and checksum come from the environment" "$compose_text" "MINION_VERSION"
assert_contains "compose: the cache is a volume" "$compose_text" "/cache"
assert_contains "compose: admin password through a secret file" "$compose_text" "MINION_ADMIN_PASSWORD_FILE"
assert_contains "compose: runs as UID 65532 (A-14)" "$compose_text" "65532"
assert_not_contains "compose: no literal password" "$compose_text" "MINION_ADMIN_PASSWORD:"
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' 2>/dev/null; then
	if python3 -c 'import sys, yaml; yaml.safe_load(open(sys.argv[1]))' "$COMPOSE" 2>/dev/null; then ok "compose: valid YAML"; else bad "compose: valid YAML"; fi
else
	echo "skip compose YAML parse (python3 yaml not installed)"
fi

if command -v shellcheck >/dev/null 2>&1; then
	if out=$(shellcheck -s sh "$SCRIPT" "$HERE/bootstrap_test.sh" 2>&1); then ok "shellcheck is clean"; else bad "shellcheck is clean" "$out"; fi
else
	echo "skip shellcheck (not installed; CI installs it)"
fi
if out=$(sh -n "$SCRIPT" 2>&1); then ok "sh -n bootstrap.sh"; else bad "sh -n bootstrap.sh" "$out"; fi
if command -v busybox >/dev/null 2>&1; then
	if out=$(busybox sh -n "$SCRIPT" 2>&1); then ok "busybox sh -n bootstrap.sh"; else bad "busybox sh -n bootstrap.sh" "$out"; fi
fi

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
