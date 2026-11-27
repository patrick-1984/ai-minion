#!/usr/bin/env bash
# check-private-names.sh: fail when an owner-internal name comes back into the
# tree: the owner's private domain and Hub host names, real internal addresses,
# device and Wi-Fi names, and the owner's personal host names. Use RFC 2606 /
# RFC 5737 examples instead (minion.example, 192.0.2.10, 198.51.100.0/24,
# 203.0.113.0/24).
#
# Allowed: the Android application ID and Kotlin package, which stays as it is,
# in its three spellings: dotted (also with a suffix, such as .adbprobe or
# .MainActivity), as a path (kotlin/.../minion) and JNI-mangled (Java_..._minion_).
# Any other use of the domain (a host name, an e-mail address, the reversed
# domain without the package) fails.
#
# Checks every tracked and every untracked, not ignored file (text only) and
# prints only file:line, never the match. The patterns are assembled from
# pieces so that this file does not match itself.
#
# Usage: scripts/check-private-names.sh [repo-root]   (default: the parent of scripts/)
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$root"
command -v perl >/dev/null 2>&1 || { echo "check-private-names: perl is required" >&2; exit 2; }

d='na''sz'
export CPN_DENY="${d}|10[.]3[.]3[.]|192[.]168[.]2[.]|orange""-sens|nebu""la|patr""yk"
export CPN_ALLOW="cc[.]${d}[.]minion\\b|cc/${d}/minion\\b|Java_cc_${d}_minion_"

status=0
git ls-files -z --cached --others --exclude-standard |
  while IFS= read -r -d '' f; do
    if [ -f "$f" ] && [ ! -L "$f" ] && grep -Iq . "$f" 2>/dev/null; then printf '%s\0' "$f"; fi
  done |
  xargs -0 -r perl -ne '
    BEGIN { $deny = qr/$ENV{CPN_DENY}/i; $allow = qr/$ENV{CPN_ALLOW}/; $bad = 0 }
    if (/$deny/) { (my $l = $_) =~ s/$allow//g; if ($l =~ $deny) { print STDERR "check-private-names: $ARGV:$.: internal name; use a neutral example (minion.example, 192.0.2.10)\n"; $bad = 1 } }
    close ARGV if eof;
    END { exit($bad ? 1 : 0) }' || status=1

if [ "$status" != 0 ]; then
  echo "check-private-names: FAIL" >&2
  exit 1
fi
echo "check-private-names: OK"
