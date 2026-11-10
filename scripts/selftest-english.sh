#!/usr/bin/env bash
# selftest-english.sh: proves that check-english.sh fails on non-English text
# in docs and Go string literals (single-line and multi-line raw strings),
# passes on plain English and symbols, and honours the allow-list, including
# trailing comments on its entries. Runs on a scratch tree; the repository is not touched.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
check="$here/check-english.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/internal/x" "$tmp/docs" "$tmp/scripts"
: >"$tmp/scripts/check-english.allow"

expect() { # expect <0|1> <description>
  local want="$1" desc="$2" got=0
  "$check" "$tmp" >/dev/null 2>&1 || got=$?
  if [ "$got" != "$want" ]; then
    echo "selftest-english: FAIL: $desc (exit $got, want $want)" >&2
    exit 1
  fi
  echo "selftest-english: ok: $desc"
}

printf 'package x\n\nvar s = "plain english -> with symbols \xe2\x86\x92 \xe2\x80\x94"\n' >"$tmp/internal/x/a.go"
printf 'English text with an arrow \xe2\x86\x92 and a dash \xe2\x80\x94\n' >"$tmp/docs/a.md"
expect 0 "English text and symbols pass"

printf 'package x\n\nvar s = "Gr\xc3\xbc\xc3\x9fe"\n' >"$tmp/internal/x/a.go"
expect 1 "accented letters in a Go string literal fail"
printf 'package x\n\nvar s = "plain"\n' >"$tmp/internal/x/a.go"

printf '\xd0\x9f\xd1\x80\xd0\xb8\xd0\xb2\xd0\xb5\xd1\x82\n' >"$tmp/docs/a.md"
expect 1 "Cyrillic text in docs fails"

echo 'docs/a.md' >"$tmp/scripts/check-english.allow"
expect 0 "an allow-listed file passes"

printf 'English\n\xd0\x9f\xd1\x80\xd0\xb8\xd0\xb2\xd0\xb5\xd1\x82\n' >"$tmp/docs/a.md"
printf 'docs/a.md:%s\n' "$(printf '\xd0\x9f\xd1\x80')" >"$tmp/scripts/check-english.allow"
expect 0 "an allow-listed line passes"

# Non-English text on the body line of a multi-line raw string (for example a
# cobra Long help text) must fail even though the line has no quote.
printf 'English\n' >"$tmp/docs/a.md"
printf 'package x\n\nconst long = `Usage:\n  Stra\xc3\x9fe \xc3\xb6ffnen\n`\n' >"$tmp/internal/x/a.go"
: >"$tmp/scripts/check-english.allow"
expect 1 "non-English text in a multi-line raw string body fails"
printf 'package x\n\nconst long = `Usage:\n  Open the door\n`\n' >"$tmp/internal/x/a.go"
expect 0 "English text in a multi-line raw string body passes"

# Trailing comments on allow-list entries (both forms) are ignored.
printf '\xd0\x9f\xd1\x80\xd0\xb8\xd0\xb2\xd0\xb5\xd1\x82\n' >"$tmp/docs/a.md"
printf '# a full-line comment\ndocs/a.md   # whole file: test data\n' >"$tmp/scripts/check-english.allow"
expect 0 "a whole-file entry with a trailing comment passes"
printf 'English\n\xd0\x9f\xd1\x80\xd0\xb8\xd0\xb2\xd0\xb5\xd1\x82\n' >"$tmp/docs/a.md"
printf 'docs/a.md:%s   # only lines with this text\n' "$(printf '\xd0\x9f\xd1\x80')" >"$tmp/scripts/check-english.allow"
expect 0 "a line entry with a trailing comment passes"
printf 'docs/a.md:%s   # only lines with this text\n' "$(printf '\xd0\xb7\xd0\xb7')" >"$tmp/scripts/check-english.allow"
expect 1 "a line entry that does not match the text still fails"
