#!/usr/bin/env bash
# Container end-to-end job (Story 9.6, AD-25): the Hub image built from this
# source by ko and the native minion binary, tested together.
#
#   test/e2e/docker/run.sh [direct|proxy|all]        (default: all)
#
# Needs docker with the compose plugin, ko, go, tmux, curl and python3. For each
# variant it starts the Hub container with a /data bind mount and a seeded admin
# (secret in a file), then runs checks_test.go (build tag dockere2e), which
# creates an invitation over REST, enrolls a native foreground Worker on a
# private tmux socket, creates a session, sends text, reads the screen, restarts
# the Hub container and asserts that the Worker reconnects.
#
# Environment:
#   E2E_IMAGE         use this existing Hub image instead of building one with ko
#   E2E_HELPER_IMAGE  helper image for chown and cleanup (default alpine:3.22, the
#                     one test/smoke uses)
#   RUNNER_TEMP       parent of the private work directory (default $TMPDIR or /tmp)
#
# Safety: nothing here touches the tmux server of the machine. This script runs
# no tmux command; the checks start their server through internal/testutil/
# tmuxtest (a private socket under $RUNNER_TEMP, killed by that socket), TMUX
# and TMUX_PANE are unset, and the cleanup removes only the compose projects and
# the files this run created. The job fails when the default tmux socket was
# created.
set -euo pipefail
unset TMUX TMUX_PANE
export GOFLAGS=-buildvcs=false

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../../.." && pwd)
which_variants=${1:-all}
case "$which_variants" in direct | proxy | all) ;; *) echo "usage: run.sh [direct|proxy|all]" >&2; exit 2 ;; esac

log() { printf 'e2e-docker: %s\n' "$*" >&2; }
die() { log "FAIL: $*"; exit 1; }
for tool in docker go tmux curl python3; do command -v "$tool" >/dev/null 2>&1 || die "$tool is required"; done
docker compose version >/dev/null 2>&1 || die "the docker compose plugin is required"
[ -n "${E2E_IMAGE:-}" ] || command -v ko >/dev/null 2>&1 || die "ko is required (or set E2E_IMAGE)"

helper=${E2E_HELPER_IMAGE:-alpine:3.22}
export E2E_HELPER_IMAGE=$helper
tmp=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
work=$(mktemp -d "$tmp/minion-e2e.XXXXXX")
chmod 700 "$work"

# The default tmux socket of this user, as it is before anything starts.
default_sock=${TMUX_TMPDIR:-/tmp}/tmux-$(id -u)/default
if [ -e "$default_sock" ] || [ -L "$default_sock" ]; then
  default_state="present:$(python3 -c 'import os,sys;print(os.lstat(sys.argv[1]).st_mtime_ns)' "$default_sock")"
else
  default_state=absent
fi

projects=()
cleanup() {
  local rc=$?
  set +e
  local p
  for p in "${projects[@]}"; do
    docker compose -p "$p" -f "$here/compose.yaml" --profile direct --profile proxy down --volumes --remove-orphans >/dev/null 2>&1
  done
  # The private tmux server of the checks belongs to tmuxtest, which kills it
  # by its own socket; this script never runs a tmux command.
  # Files written by the container user (65532) are removed by a helper
  # container, as test/smoke does.
  docker run --rm -v "$work:/w" "$helper" sh -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1
  rm -rf "$work"
  exit "$rc"
}
trap cleanup EXIT

# ------------------------------------------------------------ binaries, image
mkdir -p "$work/bin"
log "building the native minion binary and the test proxy"
(cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$work/bin/minion" ./cmd/minion &&
  CGO_ENABLED=0 go build -trimpath -o "$work/bin/proxy" ./test/e2e/docker/proxy)
chmod 755 "$work/bin" "$work/bin/minion" "$work/bin/proxy"

if [ -n "${E2E_IMAGE:-}" ]; then
  image=$E2E_IMAGE
else
  log "building the Hub image from this source with ko (loaded into the local daemon)"
  commit=${GITHUB_SHA:-local}
  # --local loads the image into the daemon as ko.local:<tag>; --bare keeps the
  # name free of the import path. The settings come from .ko.yaml, the same
  # file the platform assertion uses.
  (cd "$root" && MINION_VERSION=e2e MINION_COMMIT="${commit:0:7}" MINION_DATE=1970-01-01T00:00:00Z \
    ko build ./cmd/minion --local --bare --sbom=none --platform "linux/$(go env GOARCH)" --tags e2e >/dev/null)
  image=ko.local:e2e
fi
docker image inspect "$image" >/dev/null 2>&1 || die "the Hub image $image is not in the local daemon"
export E2E_IMAGE=$image

# The admin secret: a random value in a file (mode 0644 so that uid 65532 can
# read it through the bind mount; the directory around it is 0700).
(umask 022 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/admin.pw")
chmod 644 "$work/admin.pw"
export E2E_ADMIN_PASSWORD_FILE=$work/admin.pw

free_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }

# wait_https URL CA: /healthz answers 200 over verified TLS.
wait_https() {
  local url=$1 ca=$2
  for _ in $(seq 1 90); do
    [ -s "$ca" ] && [ "$(curl -sS --cacert "$ca" -o /dev/null -w '%{http_code}' --max-time 3 "$url/healthz" 2>/dev/null || true)" = 200 ] && return 0
    sleep 1
  done
  return 1
}

run_variant() {
  local variant=$1 proj dir svc url ca cp port
  proj="minion-e2e-$variant-$$"
  projects+=("$proj")
  dir=$work/$variant
  mkdir -p "$dir/data" "$dir/certs" "$dir/worker"
  chmod 700 "$dir/data" "$dir/worker"
  chmod 755 "$dir/certs" # the proxy writes cert.pem here as 65532; the test reads it
  export E2E_PROJECT=$proj E2E_DATA_DIR=$dir/data E2E_CERT_DIR=$dir/certs E2E_PROXY_BIN=$work/bin/proxy
  cp=(docker compose -p "$proj" -f "$here/compose.yaml" --profile "$variant")

  if [ "$variant" = direct ]; then
    svc=hub
    # compose reads the whole file whatever the profile: the proxy-only
    # variables get placeholders that no direct-profile service uses.
    export E2E_PROXY_PORT=1 E2E_PUBLIC_URL=https://localhost:1
    "${cp[@]}" up -d hub
    port=$("${cp[@]}" port hub 8443 | head -n 1 | sed 's/.*://')
    [ -n "$port" ] || die "the Hub container publishes no port"
    url=https://localhost:$port
    # The generated certificate lives in the container's /data/tls, which the
    # bind-mounted directory hides from this user: copy it out through the
    # daemon. It is the only trust root of the REST client.
    ca=$dir/ca.pem
    for _ in $(seq 1 60); do "${cp[@]}" cp hub:/data/tls/cert.pem "$ca" >/dev/null 2>&1 && break; sleep 1; done
    [ -s "$ca" ] || die "could not read the Hub certificate: $("${cp[@]}" logs hub 2>&1 | tail -n 5)"
  else
    svc=hub-proxied
    E2E_PROXY_PORT=$(free_port)
    export E2E_PROXY_PORT E2E_PUBLIC_URL=https://localhost:$E2E_PROXY_PORT
    "${cp[@]}" up -d proxy
    url=$E2E_PUBLIC_URL
    ca=$dir/certs/cert.pem
  fi
  wait_https "$url" "$ca" || { "${cp[@]}" logs 2>&1 | tail -n 30 >&2; die "the $variant Hub did not answer /healthz over verified TLS"; }
  log "$variant: Hub is up at $url"

  local restart
  restart=$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1:]))' "${cp[@]}" restart "$svc")

  log "$variant: running checks_test.go"
  if ! (cd "$root" && E2E_VARIANT=$variant E2E_HUB_URL=$url E2E_CA_FILE=$ca E2E_MINION_BIN=$work/bin/minion \
    E2E_WORK=$dir/worker E2E_RESTART_ARGV=$restart \
    E2E_DEFAULT_TMUX_SOCKET=$default_sock E2E_DEFAULT_TMUX_STATE=$default_state \
    TMPDIR=$tmp MINION_REQUIRE_TMUX=1 go test -tags dockere2e -count=1 -timeout 8m -v ./test/e2e/docker/); then
    "${cp[@]}" logs 2>&1 | tail -n 40 >&2
    die "the $variant checks failed"
  fi
  "${cp[@]}" down --volumes --remove-orphans >/dev/null
  log "$variant: passed"
}

case "$which_variants" in
  all) run_variant direct; run_variant proxy ;;
  *) run_variant "$which_variants" ;;
esac

# The run is over: the default tmux socket is as it was (absent on a clean runner).
case "$default_state" in
  absent) [ ! -e "$default_sock" ] && [ ! -L "$default_sock" ] || die "the default tmux socket $default_sock exists after the run" ;;
esac
log "all variants passed"
