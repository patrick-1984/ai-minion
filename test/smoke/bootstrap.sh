#!/usr/bin/env bash
# bootstrap.sh (smoke): run the exact release bootstrap.sh in a clean alpine and
# a clean debian container (Story 9.7, AD-24, R33). The file is called
# bootstrap.sh like the script it tests; it lives in test/smoke.
#
# Usage: test/smoke/bootstrap.sh ASSETS_DIR TAG
#
# The release is still a draft, so GitHub does not serve it. The script serves
# the verified assets from a loopback HTTP server laid out like the release
# download URLs (MINION_RELEASE_BASE); this changes only where bytes come from.
# The pinned cosign is still fetched from GitHub and verifies the real
# checksums.txt bundle against the real release workflow identity.
#
# Per distribution it checks:
#   1. exact version, cosign path: bootstrap.sh verifies and runs "minion version";
#   2. exact version with MINION_SHA256 (the archive hash from checksums.txt) in a
#      fresh cache: no cosign needed;
#   3. a wrong MINION_SHA256 is refused and nothing is installed;
#   4. an offline start (release host unreachable) runs the cached version.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"

[ $# -eq 2 ] || smoke_die "usage: bootstrap.sh ASSETS_DIR TAG"
assets=$(cd "$1" && pwd)
tag=$2
smoke_require_tag "$tag"
[ -f "$assets/bootstrap.sh" ] || smoke_die "the release has no bootstrap.sh asset"
archive=$(smoke_archive_name "$tag" linux amd64)
[ -f "$assets/$archive" ] || smoke_die "missing $archive"
sum=$(smoke_sha256 "$assets/$archive")
smoke_listed_in_checksums "$assets/checksums.txt" "$assets/$archive" ||
	smoke_die "$archive is not in the signed checksums.txt"

work=$(mktemp -d)
server_pid=""
cleanup() {
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

# <work>/site/<tag>/<assets>, served on a free loopback port.
mkdir -p "$work/site"
ln -s "$assets" "$work/site/$tag"
port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
python3 -m http.server "$port" --bind 127.0.0.1 --directory "$work/site" >"$work/http.log" 2>&1 &
server_pid=$!
smoke_wait_for 20 "the asset server" curl -fsS "http://127.0.0.1:$port/$tag/checksums.txt" -o /dev/null

# The container script (POSIX sh, runs in alpine and debian).
read -r -d '' inner <<'INNER' || true
set -eu
install_curl() {
	if command -v apk >/dev/null 2>&1; then apk add --no-cache curl ca-certificates >/dev/null
	else apt-get update -qq >/dev/null && apt-get install -y --no-install-recommends curl ca-certificates >/dev/null; fi
}
install_curl
want="version: $TAG"

# 1. Exact version, cosign path.
out=$(MINION_VERSION=$TAG MINION_CACHE_DIR=/cache sh /bootstrap.sh version)
echo "$out" | grep -Fx "$want" >/dev/null || { echo "cosign path printed: $out" >&2; exit 1; }

# 2. Exact version with MINION_SHA256 in a fresh cache.
out=$(MINION_VERSION=$TAG MINION_SHA256=$SUM MINION_CACHE_DIR=/cache2 sh /bootstrap.sh version)
echo "$out" | grep -Fx "$want" >/dev/null || { echo "sha256 path printed: $out" >&2; exit 1; }

# 3. A wrong checksum is refused and nothing is installed.
if MINION_VERSION=$TAG MINION_SHA256=0000000000000000000000000000000000000000000000000000000000000000 MINION_CACHE_DIR=/cache3 sh /bootstrap.sh version >/tmp/wrong.out 2>&1; then
	echo "a wrong MINION_SHA256 was accepted" >&2; exit 1
fi
grep -i "checksum mismatch" /tmp/wrong.out >/dev/null || { cat /tmp/wrong.out >&2; echo "no checksum mismatch message" >&2; exit 1; }
if ls /cache3/v* >/dev/null 2>&1; then echo "a cache entry exists after a refused checksum" >&2; exit 1; fi

# 4. Offline: the release host is unreachable, the cached version runs.
out=$(MINION_RELEASE_BASE=http://127.0.0.1:1 MINION_RELEASE_API=http://127.0.0.1:1 MINION_VERSION=$TAG MINION_CACHE_DIR=/cache sh /bootstrap.sh version)
echo "$out" | grep -Fx "$want" >/dev/null || { echo "offline start printed: $out" >&2; exit 1; }
echo ok
INNER

for image in alpine:3.22 debian:13; do
	smoke_log "bootstrap smoke test in $image ($tag)"
	out=$(docker run --rm --network host \
		-v "$assets/bootstrap.sh:/bootstrap.sh:ro" \
		-e "TAG=$tag" -e "SUM=$sum" \
		-e "MINION_RELEASE_BASE=http://127.0.0.1:$port" \
		"$image" sh -c "$inner") || smoke_die "bootstrap.sh failed in $image: $out"
	printf '%s\n' "$out" | tail -n 1 | grep -Fx ok >/dev/null || smoke_die "bootstrap.sh smoke script did not finish in $image: $out"
	smoke_log "bootstrap smoke test passed in $image"
done
