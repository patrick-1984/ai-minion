#!/usr/bin/env bash
# check-compose.sh: validates deploy/compose/*.yaml with "docker compose config"
# (Story 9.8). It renders the files with a dummy environment and dummy fixture
# files in a scratch directory; "config" only reads the files, it needs no
# running Docker daemon, pulls and builds nothing and starts nothing. The
# daemon address is pointed at a path that does not exist to keep it that way.
#
# Usage: scripts/check-compose.sh [repo-root]   (default: the parent of scripts/)
# DOCKER overrides the docker command (the self test uses a stand-in).
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
docker_cmd="${DOCKER:-docker}"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

shopt -s nullglob
files=("$root"/deploy/compose/*.yaml)
if [ "${#files[@]}" -eq 0 ]; then
  echo "check-compose: no deploy/compose/*.yaml found under $root" >&2
  exit 1
fi

fail=0
for f in "${files[@]}"; do
  name="$(basename "$f")"
  dir="$scratch/${name%.yaml}"
  mkdir -p "$dir"
  cp "$f" "$dir/$name"
  # Dummy fixtures for the files the Compose files mount or build from.
  printf '%s' 'dummy-not-a-secret' > "$dir/admin_password.txt"
  printf '%s' 'mnk_dummy.not-a-secret' > "$dir/invite_key.txt"
  printf '%s\n' '#!/bin/sh' > "$dir/bootstrap.sh"
  # Nothing from the caller's environment leaks in: only the documented
  # variables, with dummy values (the Worker file requires WORKER_UID/GID).
  if env -i PATH="$PATH" HOME="$scratch" DOCKER_HOST="unix://$scratch/no-daemon.sock" \
      WORKER_UID=1000 WORKER_GID=1000 MINION_VERSION=v0.6.0 \
      "$docker_cmd" compose -f "$dir/$name" config --quiet; then
    echo "check-compose: $name ok"
  else
    echo "check-compose: $name does not validate" >&2
    fail=1
  fi
done
exit "$fail"
