#!/usr/bin/env bash
# check-english.sh: fail on non-ASCII letters that are typical of non-English
# text (accented Latin, Cyrillic, Greek, CJK and so on) in
#   - every line of every *.go file under internal/ (code, comments and string
#     literals, including the body lines of multi-line raw strings such as
#     cobra Long help texts), and
#   - every file under docs/.
# Symbols and punctuation such as arrows, dashes and box-drawing characters
# are not letters and pass. Files or lines that legitimately need such text
# (for example sanitiser test data) are listed in scripts/check-english.allow.
#
# Allow-list format, one entry per line. A line that starts with "#" is a
# comment. A whitespace-preceded "#" ends an entry, so an entry may carry a
# trailing comment:
#   <path>                   the whole file is exempt
#   <path>:<fixed text>      lines of that file containing the text are exempt
#   <entry>   # reason       trailing comment, ignored
# Paths are relative to the repository root and may use shell globs.
#
# Usage: scripts/check-english.sh [repo-root]   (default: the parent of scripts/)
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$root"
allow="scripts/check-english.allow"

command -v perl >/dev/null 2>&1 || { echo "check-english: perl is required" >&2; exit 2; }

files=()
while IFS= read -r f; do files+=("$f"); done < <(
  { [ -d internal ] && find internal -type f -name '*.go'
    [ -d docs ] && find docs -type f
  } | LC_ALL=C sort
)
[ "${#files[@]}" -gt 0 ] || { echo "check-english: no files to check"; exit 0; }

# The perl program prints "path:line:text" for every offending line.
# It reads the allow-list itself so that the per-line exemptions stay exact.
violations="$(
  ALLOW_FILE="$allow" perl -CSD -Mutf8 -e '
    use strict; use warnings;
    use File::Glob qw(bsd_glob);
    my (@whole, @lines);
    if (open(my $fh, "<:encoding(UTF-8)", $ENV{ALLOW_FILE})) {
      while (my $l = <$fh>) {
        chomp $l; $l =~ s/^\s+|\s+$//g;
        next if $l eq "" || $l =~ /^#/;
        $l =~ s/\s+#.*$//;
        if ($l =~ /^([^:]+):(.+)$/) { push @lines, [$1, $2]; } else { push @whole, $l; }
      }
    }
    sub glob_re { my $g = shift; my $r = quotemeta($g); $r =~ s/\\\*\\\*/.*/g; $r =~ s/\\\*/[^\/]*/g; $r =~ s/\\\?/[^\/]/g; return qr/^$r$/; }
    @whole = map { glob_re($_) } @whole;
    @lines = map { [glob_re($_->[0]), $_->[1]] } @lines;
    for my $path (@ARGV) {
      next if grep { $path =~ $_ } @whole;
      my @lx = grep { $path =~ $_->[0] } @lines;
      open(my $in, "<:encoding(UTF-8)", $path) or next;
      my $n = 0;
      while (my $line = <$in>) {
        $n++;
        next unless $line =~ /(?![A-Za-z])\p{L}/;
        next if grep { index($line, $_->[1]) >= 0 } @lx;
        chomp $line;
        print "$path:$n:$line\n";
      }
    }
  ' "${files[@]}"
)"

if [ -n "$violations" ]; then
  echo "check-english: non-ASCII letters found (all code, comments and docs must be English):" >&2
  echo "$violations" >&2
  echo "If the text is legitimate test data, add it to $allow." >&2
  exit 1
fi
echo "check-english: ok (${#files[@]} files)"
