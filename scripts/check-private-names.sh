#!/usr/bin/env bash
# check-private-names.sh: fail when an owner-internal name comes back into the
# tree: the former private domain and app ID, real internal addresses, device
# and Wi-Fi names, and the owner's personal host names. Use RFC 2606 / RFC 5737
# examples instead (minion.example, 192.0.2.10, 198.51.100.0/24, 203.0.113.0/24).
# Checks every tracked and every untracked, not ignored file (text only).
# The patterns are assembled from pieces so that this file does not match itself.
#
# Usage: scripts/check-private-names.sh [repo-root]   (default: the parent of scripts/)
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$root"

n='na''sz'
patterns=(
  "$n"                      # the former private domain, its reversed form and the old app ID
  '10[.]3[.]3[.]'           # internal network
  '192[.]168[.]2[.]'        # internal network
  'orange''-sens'           # Wi-Fi name
  'nebu''la'                # device name
  'patr''yk'                # personal host and account names
)
regex="$(IFS='|'; echo "${patterns[*]}")"

found=0
while IFS= read -r -d '' f; do
  [ -f "$f" ] && [ ! -L "$f" ] || continue
  if out="$(grep -I -n -i -E -- "$regex" "$f")"; then
    while IFS= read -r line; do
      echo "check-private-names: $f:${line%%:*}: internal name; use a neutral example (minion.example, 192.0.2.10)" >&2
    done <<<"$out"
    found=1
  fi
done < <(git ls-files -z --cached --others --exclude-standard)

if [ "$found" = 1 ]; then
  echo "check-private-names: FAIL" >&2
  exit 1
fi
echo "check-private-names: OK"
