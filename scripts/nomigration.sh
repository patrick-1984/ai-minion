#!/usr/bin/env bash
# nomigration.sh: the tray work (tickets v3, C3) adds no SQLite migration to
# the Worker database: pause and the new-session block live in files of the
# state directory, so a newer CLI never moves an older service's database out
# of its schema. This fails when internal/worker/store/migrations holds any
# .sql file outside the pinned list of the migrations before the tray work
# (0001 to 0011), or one whose version is numerically above 11. The loader
# (internal/sqlmigrate) accepts four or more digits and parses the version as
# a number, so 00012_x.sql is version 12: names are never compared as text.
#
# Usage: scripts/nomigration.sh [repo-root]
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
last=11 # the newest Worker migration version before the tray work
pinned=(
  0001_session_counter.sql
  0002_share_selection.sql
  0003_results_audit.sql
  0004_pipe.sql
  0005_enrollment.sql
  0006_pipe_outbox_message.sql
  0007_share_revision.sql
  0008_audit_origin.sql
  0009_cert_ratchet.sql
  0010_hub_check.sql
  0011_hub_check_lineage.sql
)
dir="$root/internal/worker/store/migrations"
fail=0
for p in "${pinned[@]}"; do
  [ -f "$dir/$p" ] || { echo "nomigration: $dir/$p is missing" >&2; fail=1; }
done
shopt -s nullglob
for f in "$dir"/*.sql; do
  name="$(basename "$f")"
  known=0
  for p in "${pinned[@]}"; do
    [ "$name" = "$p" ] && known=1
  done
  if [[ "$name" =~ ^([0-9]+)_ ]]; then
    digits="${BASH_REMATCH[1]}"
    digits="${digits#"${digits%%[!0]*}"}" # strip leading zeroes: base-10, never octal
    if [ "${#digits}" -gt 9 ] || [ "${digits:-0}" -gt "$last" ]; then
      echo "nomigration: $name is Worker migration version ${digits:-0} (> $last); the tray work must not add one (tickets v3, C3)" >&2
      fail=1
      continue
    fi
  fi
  if [ "$known" = 0 ]; then
    echo "nomigration: $name is not one of the pinned Worker migrations 0001 to 0011; the tray work must not add one (tickets v3, C3)" >&2
    fail=1
  fi
done
[ "$fail" = 0 ] || exit 1
echo "nomigration: ok"
