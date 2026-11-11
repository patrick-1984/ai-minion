#!/usr/bin/env bash
# image.sh: pull the release image by digest and run the Hub from it as UID
# 65532 on three kinds of data directory (Story 9.7, AD-24, A-14, R32).
#
# Usage: test/smoke/image.sh TAG
#
# 1. The image tag must carry a cosign signature of the release workflow
#    (verified by digest), then the image is pulled by that digest.
# 2. Fresh bind-mounted data dir prepared with "chown 65532:65532": the Hub
#    starts and /healthz answers 200.
# 3. Data dir restored with "minion hub restore" from a backup of (2): the Hub
#    starts on it and /healthz answers 200.
# 4. Unprepared root-owned data dir: the Hub exits non-zero and its message
#    names the exact fix ("run: chown 65532:65532 <host dir>").
#
# SMOKE_DIGEST_OUTPUT (optional) is a file (the step's $GITHUB_OUTPUT): when
# every check passed, "digest=sha256:..." is appended to it, the digest that was
# verified and tested, so that the publish gate moves the channel tags to
# exactly that digest and not to one looked up again later.
#
# The Hub uses the image defaults (TLS auto, a self-signed certificate), so the
# probe accepts any certificate (curl -k); the admin password of the throw-away
# Hub comes from a file, never from argv or the log.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"

[ $# -eq 1 ] || smoke_die "usage: image.sh TAG"
tag=$1
smoke_require_tag "$tag"

# The exact message of the Hub for an unwritable data dir (A-14), as printed by
# internal/hub/server (CheckDataDir) for the image's data dir /data.
EXPECTED_FIX="data dir /data is not writable by uid 65532; run: chown 65532:65532 <host dir>"
UID_HUB=65532
HELPER_IMAGE=${SMOKE_HELPER_IMAGE:-alpine:3.22}

work=$(mktemp -d)
containers=()
cleanup() {
	local c
	for c in ${containers[@]+"${containers[@]}"}; do docker rm -f "$c" >/dev/null 2>&1 || true; done
	# The data dirs belong to 65532 or root: remove them through a container.
	docker run --rm -v "$work:/w" "$HELPER_IMAGE" sh -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1 || true
	rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT

# --- 1. verify and pin the digest -------------------------------------------
digest=$(docker buildx imagetools inspect "$SMOKE_IMAGE:$tag" --format '{{.Manifest.Digest}}')
printf '%s\n' "$digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || smoke_die "could not resolve the digest of $SMOKE_IMAGE:$tag (got '$digest')"
ref="$SMOKE_IMAGE@$digest"
smoke_log "image smoke test: $SMOKE_IMAGE:$tag is $digest"

if command -v cosign >/dev/null 2>&1; then
	cosign verify "$ref" \
		--certificate-identity "${SMOKE_SIGNER_WORKFLOW}@refs/tags/${tag}" \
		--certificate-oidc-issuer "$SMOKE_OIDC_ISSUER" >/dev/null ||
		smoke_die "the cosign signature of $ref does not verify for $tag"
else
	smoke_die "cosign is not installed"
fi
docker pull "$ref" >/dev/null

# The Hub's admin password: a random throw-away value in a file.
smoke_make_secret_file "$work/admin.pw"

# prepare DIR: create DIR and give it to uid 65532, the documented step.
prepare() {
	mkdir -p "$1"
	docker run --rm -v "$1:/d" "$HELPER_IMAGE" chown "$UID_HUB:$UID_HUB" /d
}

# hub_args: the docker flags that run the Hub on host dir $1.
hub_flags() {
	printf '%s\n' -u "$UID_HUB:$UID_HUB" -v "$1:/data" -v "$work/admin.pw:/run/secrets/admin.pw:ro" \
		-e MINION_DATA_DIR=/data -e MINION_ADMIN_PASSWORD_FILE=/run/secrets/admin.pw
}

# health_200 NAME: the container's /healthz answers 200 on its mapped port.
health_200() {
	local port code
	port=$(docker port "$1" 8443/tcp | head -n 1 | sed 's/.*://')
	[ -n "$port" ] || return 1
	code=$(curl -ksS -o /dev/null -w '%{http_code}' --max-time 3 "https://127.0.0.1:$port/healthz" 2>/dev/null || true)
	[ "$code" = 200 ]
}

# start_hub NAME DIR: runs the Hub detached and waits for /healthz 200.
start_hub() {
	local name=$1 dir=$2 flags=()
	while IFS= read -r l; do flags+=("$l"); done < <(hub_flags "$dir")
	containers+=("$name")
	docker run -d --name "$name" -p 127.0.0.1::8443 "${flags[@]}" "$ref" hub serve >/dev/null
	local i=0
	until health_200 "$name"; do
		i=$((i + 1))
		if [ "$i" -gt 60 ] || [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" != true ]; then
			smoke_die "the Hub in $name did not answer /healthz with 200: $(docker logs "$name" 2>&1 | tail -n 5)"
		fi
		sleep 1
	done
	smoke_log "$name: /healthz 200"
}

# --- 2. fresh data dir prepared with chown 65532:65532 ----------------------
prepare "$work/fresh"
start_hub minion-smoke-fresh "$work/fresh"
docker stop minion-smoke-fresh >/dev/null

# --- 3. data dir restored with "minion hub restore" -------------------------
prepare "$work/backup"
# The backup is encrypted: the passphrase goes through stdin into a private
# file owned by the Hub user (never argv or the log); the Hub user reads it.
passphrase=$(head -c 24 /dev/urandom | base64)
printf '%s\n' "$passphrase" | docker run --rm -i -v "$work/backup:/b" "$HELPER_IMAGE" \
	sh -c "umask 077 && cat > /b/pass && chown $UID_HUB:$UID_HUB /b/pass" ||
	smoke_die "could not create the passphrase file"
docker run --rm -u "$UID_HUB:$UID_HUB" -v "$work/fresh:/data" -v "$work/backup:/backup" \
	-e MINION_DATA_DIR=/data "$ref" hub backup --out /backup/hub.tar.gz --passphrase-file /backup/pass >/dev/null ||
	smoke_die "minion hub backup failed in the image"
prepare "$work/restored"
docker run --rm -u "$UID_HUB:$UID_HUB" -v "$work/restored:/data" -v "$work/backup:/backup:ro" \
	-e MINION_DATA_DIR=/data "$ref" hub restore /backup/hub.tar.gz --passphrase-file /backup/pass >/dev/null ||
	smoke_die "minion hub restore failed in the image"
start_hub minion-smoke-restored "$work/restored"
docker stop minion-smoke-restored >/dev/null

# --- 4. unprepared root-owned data dir --------------------------------------
mkdir -p "$work/unprepared-parent"
docker run --rm -v "$work/unprepared-parent:/p" "$HELPER_IMAGE" \
	sh -c 'mkdir /p/data && chown 0:0 /p/data && chmod 755 /p/data'
flags=()
while IFS= read -r l; do flags+=("$l"); done < <(hub_flags "$work/unprepared-parent/data")
set +e
out=$(timeout 60 docker run --rm "${flags[@]}" "$ref" hub serve 2>&1)
code=$?
set -e
[ "$code" -ne 0 ] || smoke_die "the Hub started on a root-owned data dir; it must refuse"
[ "$code" -ne 124 ] || smoke_die "the Hub kept running on a root-owned data dir; it must exit"
printf '%s\n' "$out" | grep -Fq "$EXPECTED_FIX" ||
	smoke_die "the exit message does not print the exact fix '$EXPECTED_FIX'; it was: $out"
smoke_log "unprepared data dir refused with the exact fix (exit $code)"
if [ -n "${SMOKE_DIGEST_OUTPUT:-}" ]; then
	printf 'digest=%s\n' "$digest" >>"$SMOKE_DIGEST_OUTPUT"
fi
smoke_log "image smoke test passed"
