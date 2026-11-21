#!/usr/bin/env bash
# pin.sh: the draft release and the image are still exactly what the smoke
# tests tested (Story 9.7, AD-24: publish only after the install smoke tests of
# the exact artifacts).
#
# Usage: test/smoke/pin.sh TAG ASSET_MANIFEST_FILE IMAGE_DIGEST
#
# publish.yml records the complete asset set of the draft when it resolves it
# (test/smoke/common.sh smoke_asset_manifest: "<sha256>  <name>" for every asset,
# the archives and packages as well as checksums.txt, release-manifest.json,
# bootstrap.sh, install.sh, install.ps1 and every cosign bundle), and image.sh reports the digest it
# tested. This script runs under the release locks right before the draft is
# published and fails unless
#   - the release has exactly the recorded assets (none missing, none added,
#     none with other bytes; a checksums.txt that did not change proves
#     nothing about the files that checksums.txt does not list), and
#   - the signed files carry valid cosign bundles of the release workflow at
#     refs/tags/TAG and every other asset is listed in the signed checksums.txt,
#     so a bad signature stops the release while it is still a draft, and
#   - the signed release-manifest.json names SMOKE_IMAGE@IMAGE_DIGEST as its
#     image (the manifest attests the tested digest, not another one), and
#   - SMOKE_IMAGE:TAG still resolves to IMAGE_DIGEST.
# GH_TOKEN must be able to read the draft; docker must be logged in to the
# registry; cosign must be installed.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"

[ $# -eq 3 ] || smoke_die "usage: pin.sh TAG ASSET_MANIFEST_FILE IMAGE_DIGEST"
tag=$1 manifest=$2 tested=$3
smoke_require_tag "$tag"
printf '%s\n' "$tested" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	smoke_die "the tested image digest '$tested' is not sha256:<64 hex digits>"
[ -s "$manifest" ] || smoke_die "the recorded asset manifest '$manifest' is missing or empty"
command -v gh >/dev/null 2>&1 || smoke_die "gh is not installed"
command -v cosign >/dev/null 2>&1 || smoke_die "cosign is not installed"

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
gh release download "$tag" --repo "$SMOKE_REPO" --dir "$dir" --clobber
smoke_check_asset_manifest "$dir" "$manifest"
smoke_verify_all_assets "$dir" "$tag"

# The signed manifest must attest exactly the tested image.
want_repo=$SMOKE_IMAGE
# Compared in python on exact values: no whitespace or newline is tolerated.
python3 -I -c 'import json, sys
i = json.load(open(sys.argv[1])).get("image") or {}
sys.exit(0 if (i.get("repository"), i.get("digest")) == (sys.argv[2], sys.argv[3]) else 1)' \
	"$dir/release-manifest.json" "$want_repo" "$tested" ||
	smoke_die "release-manifest.json does not name exactly the image '$want_repo@$tested' that the smoke tests tested"

now=$(docker buildx imagetools inspect "$SMOKE_IMAGE:$tag" --format '{{.Manifest.Digest}}')
[ "$now" = "$tested" ] ||
	smoke_die "$SMOKE_IMAGE:$tag is $now now, the smoke tests tested $tested: the image was pushed again"
smoke_log "release $tag ($(wc -l <"$manifest" | tr -d ' ') assets) and $SMOKE_IMAGE:$tag are still the tested ones ($tested)"
