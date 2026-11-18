#!/usr/bin/env bash
# nomigration.sh: the tray work (tickets v3, C3) adds no SQLite migration to
# the Worker database: pause and the new-session block live in files of the
# state directory, so a newer CLI never moves an older service's database out
# of its schema. This fails when internal/worker/store/migrations holds a
# migration newer than the last one before the tray work.
#
# Usage: scripts/nomigration.sh [repo-root]
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
last="0011_hub_check_lineage.sql" # the newest Worker migration before the tray work
dir="$root/internal/worker/store/migrations"
[ -f "$dir/$last" ] || { echo "nomigration: $dir/$last is missing" >&2; exit 1; }
fail=0
for f in "$dir"/*.sql; do
  name="$(basename "$f")"
  if [[ "$name" > "$last" ]]; then
    echo "nomigration: $name is a new Worker migration; the tray work must not add one (tickets v3, C3)" >&2
    fail=1
  fi
done
[ "$fail" = 0 ] || exit 1
echo "nomigration: ok"
