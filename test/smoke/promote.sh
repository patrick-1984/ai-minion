#!/usr/bin/env bash
# promote.sh: publish a tested draft release and move the image channels, with
# a rollback (Story 9.7, AD-24, A-13).
#
# Usage: test/smoke/promote.sh TAG ASSET_MANIFEST_FILE IMAGE_DIGEST
#
# Runs in the publish job of publish.yml after pin.sh, inside the publish
# locks (the per-tag group shared with release.yml and the global channel
# group), so that the live state read here cannot change while this script
# runs. Environment: GH_TOKEN (edit releases), docker logged in to the
# registry with write access, cosign, and optionally SMOKE_IMAGE,
# SMOKE_PUBLIC_BASE (default https://github.com/$SMOKE_REPO/releases/download).
#
# Order, so that every check that can fail runs before anything is changed:
#   1. Read the live state: the release is a draft or public, and the plan
#      (test/smoke/channels.sh) comes from the releases that are public NOW.
#   2. Preflight (nothing is changed yet): verify the signature of the tested
#      image digest, and record what has to be restored: the GitHub "latest"
#      release, the original prerelease flag and the digest of every image tag
#      that would move.
#   3. Publish the draft (a candidate as a prerelease, a final release as a
#      normal release, "latest" only when the plan says so).
#   4. Verify the public release: flags, the exact asset names, a download of
#      every asset without a token compared with the recorded asset set, the
#      cosign bundles.
#   5. Final release only: move the image tags in the plan to the tested
#      digest and verify digest and signature of every moved tag. A candidate
#      moves no tag.
#
# Rollback: any failure after step 2 restores what this run changed: first the
# image tags (to the digests recorded in step 2), then the release (back to a
# draft with its original prerelease flag, and the previous "latest" release
# gets its designation back). A release that was already public when this run
# started is never unpublished; only the channels are restored. A rollback
# step that fails is reported, the script exits non-zero either way.
#
# Residual risk, by design: a registry cannot delete one tag. An image tag
# that did not exist before this run (the first release of a line) cannot be
# removed again; it is left pointing at the signed, tested digest and the
# rollback says so.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"

[ $# -eq 3 ] || smoke_die "usage: promote.sh TAG ASSET_MANIFEST_FILE IMAGE_DIGEST"
tag=$1 manifest=$2 digest=$3
smoke_require_tag "$tag"
printf '%s\n' "$digest" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	smoke_die "the tested image digest '$digest' is not sha256:<64 hex digits>"
[ -s "$manifest" ] || smoke_die "the recorded asset manifest '$manifest' is missing or empty"
for c in gh docker cosign curl; do
	command -v "$c" >/dev/null 2>&1 || smoke_die "$c is not installed"
done
public_base=${SMOKE_PUBLIC_BASE:-https://github.com/${SMOKE_REPO}/releases/download}
identity="${SMOKE_SIGNER_WORKFLOW}@refs/tags/${tag}"

# What this run changed (the rollback undoes exactly this).
we_published=0
channels_touched=0
orig_prerelease=""
prev_latest=""
declare -A prev_digest=()
image_tags=""
github_latest=false
pub=""

absent_ok() {
	# absent_ok PATTERN CMD...: prints the output of CMD. When CMD fails with
	# an error that matches PATTERN (the thing does not exist), prints nothing
	# and succeeds; any other failure (network, permission) fails closed.
	local pattern=$1 out err
	shift
	err=$(mktemp)
	if out=$("$@" 2>"$err"); then
		rm -f "$err"
		printf '%s\n' "$out"
		return 0
	fi
	if grep -Eiq "$pattern" "$err"; then
		rm -f "$err"
		return 0
	fi
	cat "$err" >&2
	rm -f "$err"
	return 1
}

release_row() {
	# "<draft>\t<prerelease>" of the release of the tag; exactly one release.
	local rows
	rows=$(gh api "repos/${SMOKE_REPO}/releases" --paginate \
		--jq ".[] | select(.tag_name == \"${tag}\") | [.draft, .prerelease] | @tsv")
	[ -n "$rows" ] && [ "$(printf '%s\n' "$rows" | wc -l | tr -d ' ')" -eq 1 ] ||
		smoke_die "expected exactly one release for $tag"
	printf '%s\n' "$rows"
}

restore_channels() {
	local t failed=0 want got
	[ "$channels_touched" -eq 1 ] || return 0
	for t in $image_tags; do
		want=${prev_digest[$t]:-}
		if [ -z "$want" ]; then
			smoke_log "rollback: $SMOKE_IMAGE:$t did not exist before and a registry cannot delete one tag; it stays at the tested digest $digest"
			continue
		fi
		if docker buildx imagetools create --tag "$SMOKE_IMAGE:$t" "$SMOKE_IMAGE@$want" &&
			[ "$(docker buildx imagetools inspect "$SMOKE_IMAGE:$t" --format '{{.Manifest.Digest}}')" = "$want" ]; then
			smoke_log "rollback: $SMOKE_IMAGE:$t is $want again"
		else
			smoke_log "rollback FAILED: could not restore $SMOKE_IMAGE:$t to $want"
			failed=1
		fi
	done
	return "$failed"
}

restore_release() {
	local failed=0
	[ "$we_published" -eq 1 ] || return 0
	if gh release edit "$tag" --repo "$SMOKE_REPO" --draft=true --prerelease="$orig_prerelease"; then
		smoke_log "rollback: release $tag is a draft again"
	else
		smoke_log "rollback FAILED: could not turn release $tag back into a draft"
		failed=1
	fi
	if [ -n "$prev_latest" ] && [ "$prev_latest" != "$tag" ] && [ "$github_latest" = true ]; then
		if gh release edit "$prev_latest" --repo "$SMOKE_REPO" --latest=true; then
			smoke_log "rollback: $prev_latest is the latest release again"
		else
			smoke_log "rollback FAILED: could not make $prev_latest the latest release again"
			failed=1
		fi
	fi
	return "$failed"
}

on_exit() {
	local rc=$? incomplete
	trap - EXIT
	[ -z "$pub" ] || rm -rf "$pub"
	if [ "$rc" -ne 0 ] && { [ "$we_published" -eq 1 ] || [ "$channels_touched" -eq 1 ]; }; then
		smoke_log "promotion of $tag failed (exit $rc): rolling back what this run changed"
		# Both run, whatever the first one returns.
		incomplete=0
		restore_channels || incomplete=1
		restore_release || incomplete=1
		if [ "$incomplete" -eq 1 ]; then
			smoke_log "ROLLBACK INCOMPLETE: check the release and the image tags of $tag by hand"
		fi
	fi
	exit "$rc"
}
trap on_exit EXIT
trap 'exit 143' TERM INT

# --- 1. live state and plan ------------------------------------------------------
row=$(release_row)
state=published
case "$row" in true*) state=draft ;; esac
orig_prerelease=$(printf '%s\n' "$row" | cut -f2)
plan=$(gh release list --repo "$SMOKE_REPO" --limit 1000 --json tagName,isDraft,isPrerelease \
	--jq '.[] | select(.isDraft == false and .isPrerelease == false) | .tagName' |
	"$here/channels.sh" "$tag")
printf '%s\n' "$plan"
kind=$(printf '%s\n' "$plan" | sed -n 's/^kind=//p')
prerelease=$(printf '%s\n' "$plan" | sed -n 's/^prerelease=//p')
github_latest=$(printf '%s\n' "$plan" | sed -n 's/^github_latest=//p')
image_tags=$(printf '%s\n' "$plan" | sed -n 's/^image_tags=//p')
case "$kind:$prerelease:$github_latest" in
rc:true:false | final:false:true | final:false:false) ;;
*) smoke_die "unexpected plan for $tag: kind=$kind prerelease=$prerelease github_latest=$github_latest" ;;
esac

# --- 2. preflight: nothing is changed yet ----------------------------------------
cosign verify "$SMOKE_IMAGE@$digest" --certificate-identity "$identity" \
	--certificate-oidc-issuer "$SMOKE_OIDC_ISSUER" >/dev/null ||
	smoke_die "the signature of $SMOKE_IMAGE@$digest does not verify for $tag"
if [ "$github_latest" = true ]; then
	prev_latest=$(absent_ok 'Not Found|HTTP 404' gh api "repos/${SMOKE_REPO}/releases/latest" --jq .tag_name) ||
		smoke_die "could not read the current latest release"
fi
for t in $image_tags; do
	prev_digest[$t]=$(absent_ok 'not found|manifest unknown|no such manifest|name unknown' \
		docker buildx imagetools inspect "$SMOKE_IMAGE:$t" --format '{{.Manifest.Digest}}') ||
		smoke_die "could not read the current digest of $SMOKE_IMAGE:$t"
	smoke_log "preflight: $SMOKE_IMAGE:$t is '${prev_digest[$t]:-absent}' now"
done
smoke_log "preflight: previous latest release '${prev_latest:-none}', original prerelease flag $orig_prerelease"

# --- 3. publish ------------------------------------------------------------------
if [ "$state" = published ]; then
	smoke_log "release $tag is already public; not touching it again"
else
	we_published=1 # set first: an edit that fails after it took effect is rolled back too
	gh release edit "$tag" --repo "$SMOKE_REPO" --draft=false \
		--prerelease="$prerelease" --latest="$github_latest"
fi

# --- 4. the public release -------------------------------------------------------
now=$(release_row)
[ "$now" = "$(printf 'false\t%s' "$prerelease")" ] ||
	smoke_die "unexpected release flags (draft, prerelease): $now"
want_names=$(smoke_asset_names_from_manifest "$manifest" | LC_ALL=C sort)
have_names=$(gh api "repos/${SMOKE_REPO}/releases" --paginate \
	--jq ".[] | select(.tag_name == \"${tag}\") | .assets[].name" | LC_ALL=C sort)
[ "$want_names" = "$have_names" ] || smoke_die "the public release has other assets than the tested ones"
pub=$(mktemp -d)
# What "minion update --version" and bootstrap.sh do: download without
# credentials, compare with the tested asset set and verify the signatures.
for f in $want_names; do
	curl -fsSL --retry 5 --retry-all-errors -o "$pub/$f" "${public_base}/${tag}/${f}" ||
		smoke_die "could not download $f of the public release $tag"
done
smoke_check_asset_manifest "$pub" "$manifest"
smoke_verify_all_assets "$pub" "$tag"

# --- 5. the image channels (final release only) -----------------------------------
if [ "$kind" = final ]; then
	if [ -z "$image_tags" ]; then
		smoke_log "no channel moves: a newer final release owns every channel"
	else
		channels_touched=1 # set first: a create that fails half way is rolled back too
		args=()
		for t in $image_tags; do args+=(--tag "$SMOKE_IMAGE:$t"); done
		docker buildx imagetools create "${args[@]}" "$SMOKE_IMAGE@$digest"
		for t in $image_tags; do
			got=$(docker buildx imagetools inspect "$SMOKE_IMAGE:$t" --format '{{.Manifest.Digest}}')
			[ "$got" = "$digest" ] || smoke_die "$SMOKE_IMAGE:$t is $got, expected $digest"
			cosign verify "$SMOKE_IMAGE:$t" --certificate-identity "$identity" \
				--certificate-oidc-issuer "$SMOKE_OIDC_ISSUER" >/dev/null ||
				smoke_die "the signature of $SMOKE_IMAGE:$t does not verify"
			smoke_log "moved $SMOKE_IMAGE:$t to $digest (signature verified)"
		done
	fi
else
	smoke_log "release candidate: no channel tag and no image tag other than $tag moves"
fi
smoke_log "promoted $tag ($kind)"
