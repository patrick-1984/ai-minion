#!/usr/bin/env bash
# fetch.sh: download the exact assets of a draft (or published) release and
# verify them (Story 9.7, AD-24).
#
# Usage: test/smoke/fetch.sh TAG DIR [PATTERN...]
#
# Downloads every asset of release TAG into DIR (GH_TOKEN must be able to read
# the draft), verifies the cosign bundle of checksums.txt against the release
# workflow identity, and the bundles of bootstrap.sh and release-manifest.json.
# With patterns (shell globs on file names, for example '*_linux_amd64.tar.gz')
# only the matching assets are downloaded in addition to the signature files;
# every downloaded archive and package is then checked against checksums.txt.
#
# SMOKE_CHECKSUMS_SHA256 (optional, 64 hex digits) pins the draft: the
# downloaded checksums.txt must have exactly that sha256 (publish.yml records
# it when it resolves the draft), otherwise the draft is not the release that
# the publish gate resolved and nothing is installed or tested.
#
# GH_TOKEN is only for this script: the workflow gives it to the step that runs
# fetch.sh and to no step that executes a downloaded artifact.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"

[ $# -ge 2 ] || smoke_die "usage: fetch.sh TAG DIR [PATTERN...]"
tag=$1 dir=$2
shift 2
smoke_require_tag "$tag"
command -v gh >/dev/null 2>&1 || smoke_die "gh is not installed"
command -v cosign >/dev/null 2>&1 || smoke_die "cosign is not installed"

mkdir -p "$dir"
args=(--repo "$SMOKE_REPO" --dir "$dir" --clobber
	--pattern 'checksums.txt' --pattern 'checksums.txt.sigstore.json'
	--pattern 'bootstrap.sh' --pattern 'bootstrap.sh.sigstore.json'
	--pattern 'release-manifest.json' --pattern 'release-manifest.json.sigstore.json')
if [ $# -eq 0 ]; then
	args=(--repo "$SMOKE_REPO" --dir "$dir" --clobber)
else
	for p in "$@"; do args+=(--pattern "$p"); done
fi
gh release download "$tag" "${args[@]}"

[ -f "$dir/checksums.txt" ] || smoke_die "release $tag has no checksums.txt"
if [ -n "${SMOKE_CHECKSUMS_SHA256:-}" ]; then
	smoke_check_pinned_checksums "$dir/checksums.txt" "$SMOKE_CHECKSUMS_SHA256"
fi
# Everything that is neither a signature file nor a signed-by-itself file nor
# the checksum list must appear in checksums.txt (archives, packages, SBOMs).
files=()
for f in "$dir"/*; do
	[ -f "$f" ] || continue
	b=$(basename "$f")
	case "$b" in
	checksums.txt | *.sigstore.json | bootstrap.sh | release-manifest.json) continue ;;
	esac
	files+=("$b")
done
smoke_verify_release_assets "$dir" "$tag" ${files[@]+"${files[@]}"}
