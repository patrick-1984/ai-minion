#!/usr/bin/env bash
# channels.sh: the publish plan of a release (Story 9.7, AD-24, A-13).
#
# Usage: test/smoke/channels.sh TAG < published-final-tags
#
# TAG is the release being published (vX.Y.Z or vX.Y.Z-rc.N). Standard input
# lists the tags of the releases that are already public and final (one per
# line; anything that is not vX.Y.Z is ignored, so a release list with
# prereleases can be piped in as it is). Prints key=value lines (the format of
# $GITHUB_OUTPUT):
#
#   kind=rc|final       a release candidate or a final release
#   prerelease=true|false  publish as a GitHub prerelease
#   github_latest=true|false  mark it as the repository's latest release
#   image_tags=...      the image tags to move, space separated, to the
#                       digest of the tag; empty for a release candidate
#
# A release candidate is a public prerelease that moves no channel tag and no
# image tag other than its own vX.Y.Z-rc.N (the one goreleaser already pushed),
# so image_tags is empty. A final release moves X.Y, X and latest, but a
# channel never moves backwards: X.Y moves only when no newer final release of
# that minor line is public, X only when no newer final release of that major
# is public, and latest only when no newer final release exists at all (a fix
# for an old line must not take over "latest").
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"

[ $# -eq 1 ] || smoke_die "usage: channels.sh TAG < published-final-tags"
tag=$1
smoke_require_tag "$tag"

case "$tag" in
*-rc.*)
	printf 'kind=rc\nprerelease=true\ngithub_latest=false\nimage_tags=\n'
	exit 0
	;;
esac

ver=${tag#v}
major=${ver%%.*}
rest=${ver#*.}
minor=${rest%%.*}

published=$(grep -E '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || true)

# newer_in PREFIX: succeeds when a published final tag that starts with PREFIX
# (v1., v1.4. or "" for any) is newer than TAG.
newer_in() {
	local best
	best=$(
		{
			printf '%s\n' "$published" | awk -v p="v$1" 'index($0, p) == 1' || true
			printf '%s\n' "$tag"
		} | grep -v '^$' | sort -V | tail -n 1
	)
	[ "$best" != "$tag" ]
}

tags=""
newer_in "$major.$minor." || tags="$major.$minor"
newer_in "$major." || tags="$tags${tags:+ }$major"
latest=false
if ! newer_in ""; then
	tags="$tags${tags:+ }latest"
	latest=true
fi
printf 'kind=final\nprerelease=false\ngithub_latest=%s\nimage_tags=%s\n' "$latest" "$tags"
