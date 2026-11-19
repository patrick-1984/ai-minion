#!/usr/bin/env bash
# selftest-nomigration.sh: proves that nomigration.sh passes on the pinned
# Worker migrations 0001 to 0011 and fails on any other migration file,
# including a numerically newer version with extra leading zeroes (00012),
# a second file for an existing version and a missing pinned file. Runs on a
# scratch tree; the repository is not touched.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
check="$here/nomigration.sh"
src="$here/../internal/worker/store/migrations"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
dir="$tmp/internal/worker/store/migrations"

reset() { rm -rf "$dir"; mkdir -p "$dir"; cp "$src"/0*.sql "$dir/"; }

expect() { # expect <0|1> <description>
  local want="$1" desc="$2" got=0
  "$check" "$tmp" >/dev/null 2>&1 || got=$?
  if [ "$got" != "$want" ]; then
    echo "selftest-nomigration: FAIL: $desc (exit $got, want $want)" >&2
    exit 1
  fi
  echo "selftest-nomigration: ok: $desc"
}

reset
expect 0 "the pinned migrations 0001 to 0011 pass"
echo "SELECT 1;" >"$dir/0012_review_check.sql"
expect 1 "a valid 0012 migration fails"
reset
echo "SELECT 1;" >"$dir/00012_review_check.sql"
expect 1 "version 12 written with extra leading zeroes (00012) fails"
reset
echo "SELECT 1;" >"$dir/0000000000000000099_big.sql"
expect 1 "a long zero-padded version 99 fails"
reset
echo "SELECT 1;" >"$dir/0011_second.sql"
expect 1 "a second file for version 11 fails"
reset
rm "$dir/0011_hub_check_lineage.sql"
expect 1 "a missing pinned migration fails"
