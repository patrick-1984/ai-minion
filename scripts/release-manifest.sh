#!/usr/bin/env bash
# release-manifest.sh: writes release-manifest.json for a release (Story 9.1,
# AD-24, A-10). "minion update" (Story 9.2) reads it to refuse an update whose
# supported protocol range does not overlap the Hub's range.
#
# Usage:
#   scripts/release-manifest.sh --version vX.Y.Z[-rc.N] \
#       --protocol-min MAJOR.MINOR --protocol-max MAJOR.MINOR \
#       --checksums dist/checksums.txt [--extra FILE]... [--output FILE]
#
#   --checksums FILE  the goreleaser checksums.txt ("<sha256>  <name>" lines)
#   --extra FILE      a file that is not in checksums.txt (for example
#                     bootstrap.sh); its sha256 is computed here. Repeatable.
#   --output FILE     default: release-manifest.json (use - for stdout)
#
# Output (keys in this order, artifacts sorted by name, so the output is
# deterministic):
#   {
#     "schema": 1,
#     "version": "v0.1.0",
#     "protocol_range": {"min": "3.0", "max": "3.0"},
#     "artifacts": [{"name": "minion_0.1.0_linux_amd64.tar.gz", "sha256": "..."}]
#   }
#
# The manifest's own digest cannot be inside itself: the workflow signs the
# manifest with cosign keyless, next to the signed checksums.txt.
#
# Consumer contract (Story 9.2, "minion update"):
#   (a) The release assets are named "release-manifest.json" and
#       "release-manifest.json.sigstore.json" (the cosign bundle).
#   (b) The manifest is NOT covered by checksums.txt. A consumer must verify
#       the manifest's own bundle: the certificate identity is the release
#       workflow (https://github.com/<owner>/<repo>/.github/workflows/release.yml
#       at the source ref refs/tags/<version>) and the OIDC issuer is
#       https://token.actions.githubusercontent.com.
#   (c) protocol_range.min and protocol_range.max are "MAJOR.MINOR" strings
#       within one major version.
#   (d) artifacts[] holds {name, sha256} objects sorted by name.
set -euo pipefail

die() { echo "release-manifest: $*" >&2; exit 2; }
usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; }

version="" pmin="" pmax="" checksums="" output="release-manifest.json"
extras=()
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--version | --protocol-min | --protocol-max | --checksums | --extra | --output)
		[ $# -ge 2 ] || die "$1 needs a value"
		;;
	*) usage; die "unknown argument: $1" ;;
	esac
	case "$1" in
	--version) version="$2" ;;
	--protocol-min) pmin="$2" ;;
	--protocol-max) pmax="$2" ;;
	--checksums) checksums="$2" ;;
	--extra) extras+=("$2") ;;
	--output) output="$2" ;;
	esac
	shift 2
done

[ -n "$version" ] || die "--version is required"
[ -n "$pmin" ] || die "--protocol-min is required"
[ -n "$pmax" ] || die "--protocol-max is required"
[ -n "$checksums" ] || die "--checksums is required"
[ -f "$checksums" ] || die "no such file: $checksums"
command -v python3 >/dev/null 2>&1 || die "python3 is required"

# Digests of the extra files are computed here and handed over as "sha  name".
extra_lines=""
for f in "${extras[@]+"${extras[@]}"}"; do
	[ -f "$f" ] || die "no such extra file: $f"
	sum="$(sha256sum "$f" | awk '{print $1}')"
	extra_lines+="$sum  $(basename "$f")"$'\n'
done

EXTRA_LINES="$extra_lines" python3 -I - "$version" "$pmin" "$pmax" "$checksums" "$output" <<'PY'
import json
import os
import re
import sys

version, pmin, pmax, checksums, output = sys.argv[1:6]


def die(msg):
    print("release-manifest: " + msg, file=sys.stderr)
    sys.exit(2)


if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?", version):
    die("version must be vX.Y.Z or vX.Y.Z-rc.N, got " + repr(version))


def proto(label, value):
    m = re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", value)
    if not m:
        die(label + " must be MAJOR.MINOR, got " + repr(value))
    return int(m.group(1)), int(m.group(2))


lo, hi = proto("--protocol-min", pmin), proto("--protocol-max", pmax)
if lo[0] != hi[0]:
    die("the protocol range must stay inside one major version")
if lo > hi:
    die("--protocol-min is greater than --protocol-max")

line_re = re.compile(r"([0-9a-f]{64}) [ *](\S.*)")
artifacts = {}


def add(line, source):
    m = line_re.fullmatch(line)
    if not m:
        die("malformed checksum line in " + source + ": " + repr(line))
    sha, name = m.groups()
    if "/" in name or name in (".", "..") or name.endswith("/"):
        die("artifact name must be a plain file name: " + repr(name))
    if name in artifacts and artifacts[name] != sha:
        die("conflicting digests for " + name)
    artifacts[name] = sha


with open(checksums, encoding="utf-8") as fh:
    for raw in fh:
        raw = raw.rstrip("\n")
        if raw.strip():
            add(raw, checksums)
for raw in os.environ.get("EXTRA_LINES", "").splitlines():
    if raw.strip():
        add(raw, "--extra")

if not artifacts:
    die("no artifacts: " + checksums + " is empty")

doc = {
    "schema": 1,
    "version": version,
    "protocol_range": {"min": pmin, "max": pmax},
    "artifacts": [{"name": n, "sha256": artifacts[n]} for n in sorted(artifacts)],
}
text = json.dumps(doc, indent=2) + "\n"
if output == "-":
    sys.stdout.write(text)
else:
    with open(output, "w", encoding="utf-8") as fh:
        fh.write(text)
PY
