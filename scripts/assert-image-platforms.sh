#!/usr/bin/env bash
# assert-image-platforms.sh: asserts that the release image is a full OCI image
# index with every required platform (Story 9.1, AD-24, A-12).
#
# The assertion inspects a FULL OCI index. It never looks at the goreleaser
# snapshot, because a snapshot loads one host image into the Docker daemon.
#
# Usage:
#   scripts/assert-image-platforms.sh [--layout DIR] [--platforms LIST]
#
#   --layout DIR      inspect an existing OCI image layout (no build, no network)
#   --platforms LIST  comma separated, default linux/amd64,linux/arm64,linux/arm/v7
#
# Without --layout the script builds the image with ko into a disposable OCI
# layout (ko --oci-layout-path, nothing is pushed, no registry and no Docker
# daemon are needed) using .ko.yaml, and removes the layout at the end. ko
# v0.19.1 must be on PATH (CI: ko-build/setup-ko). The base image is pulled
# from gcr.io.
#
# Platform names are os/arch, plus /variant for arm only (ko records "v8" as the
# arm64 variant; that is the same platform as linux/arm64).
#
# Exit codes: 0 all platforms present, 1 a platform or a blob is missing or the
# layout is not an image index, 2 usage or tool error.
set -euo pipefail

default_platforms="linux/amd64,linux/arm64,linux/arm/v7"
platforms="$default_platforms"
layout=""

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; }
die() { echo "assert-image-platforms: $*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--layout) [ $# -ge 2 ] || die "--layout needs a directory"; layout="$2"; shift 2 ;;
	--platforms) [ $# -ge 2 ] || die "--platforms needs a list"; platforms="$2"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) usage; die "unknown argument: $1" ;;
	esac
done

command -v python3 >/dev/null 2>&1 || die "python3 is required"

tmp=""
cleanup() { [ -z "$tmp" ] || rm -rf "$tmp"; }
trap cleanup EXIT

if [ -z "$layout" ]; then
	command -v ko >/dev/null 2>&1 || die "ko is required to build the image (or pass --layout DIR)"
	root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
	# ko validates the layout path like a repository name: lower case only, so
	# mktemp (mixed case) cannot be used.
	tmp="/tmp/assert-image-platforms-$$-$RANDOM"
	mkdir -m 700 "$tmp" || die "cannot create $tmp"
	layout="$tmp/layout"
	echo "assert-image-platforms: building the image index with ko into $layout"
	(
		cd "$root"
		export MINION_VERSION="${MINION_VERSION:-vtest}"
		export MINION_COMMIT="${MINION_COMMIT:-$(git rev-parse --short=7 HEAD 2>/dev/null || echo dev)}"
		export MINION_DATE="${MINION_DATE:-1970-01-01T00:00:00Z}"
		export KO_DOCKER_REPO="${KO_DOCKER_REPO:-example.invalid/minion}"
		ko build ./cmd/minion --bare --push=false --sbom=none \
			--platform="$platforms" --oci-layout-path "$layout" --tags "$MINION_VERSION"
	) >&2
fi

[ -d "$layout" ] || die "no such layout directory: $layout"

python3 -I - "$layout" "$platforms" <<'PY'
import json
import os
import sys

layout, wanted = sys.argv[1], [p for p in sys.argv[2].split(",") if p]
INDEX = ("application/vnd.oci.image.index.v1+json",
         "application/vnd.docker.distribution.manifest.list.v2+json")
IMAGE = ("application/vnd.oci.image.manifest.v1+json",
         "application/vnd.docker.distribution.manifest.v2+json")


def fail(msg):
    print("assert-image-platforms: FAIL: " + msg, file=sys.stderr)
    sys.exit(1)


def blob(digest):
    algo, _, hexa = digest.partition(":")
    if algo != "sha256" or not hexa.isalnum():
        fail("unsupported digest " + digest)
    path = os.path.join(layout, "blobs", algo, hexa)
    if not os.path.isfile(path):
        fail("blob " + digest + " is missing from the layout")
    with open(path, "rb") as fh:
        return json.load(fh)


def name(platform):
    osn, arch = platform.get("os", ""), platform.get("architecture", "")
    if not osn or not arch or osn == "unknown" or arch == "unknown":
        return None  # attestation or other non-runnable entry
    variant = platform.get("variant", "")
    return osn + "/" + arch + ("/" + variant if arch == "arm" and variant else "")


try:
    with open(os.path.join(layout, "index.json"), "rb") as fh:
        top = json.load(fh)
except (OSError, ValueError) as exc:
    fail("cannot read index.json: " + str(exc))

found, indexes = set(), 0


def walk(descriptor, depth=0):
    global indexes
    if depth > 4:
        fail("index nesting is too deep")
    media = descriptor.get("mediaType", "")
    if media in INDEX:
        indexes += 1
        doc = blob(descriptor["digest"])
        for child in doc.get("manifests", []):
            plat = child.get("platform")
            if plat is not None:
                n = name(plat)
                if n is None:
                    continue
                # the platform entry must point at a real manifest
                blob(child["digest"])
                found.add(n)
            else:
                walk(child, depth + 1)
    elif media not in IMAGE:
        fail("unexpected media type " + repr(media))


for top_desc in top.get("manifests", []):
    walk(top_desc)

if indexes == 0:
    fail("the layout holds no image index (a single-platform image is not enough)")

missing = [p for p in wanted if p not in found]
print("assert-image-platforms: found " + ", ".join(sorted(found)))
if missing:
    fail("missing platform(s): " + ", ".join(missing))
print("assert-image-platforms: ok, all of " + ", ".join(wanted) + " are present")
PY
