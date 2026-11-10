#!/usr/bin/env bash
# common.sh: helpers shared by the install smoke tests (Story 9.7, AD-24).
# Source it; it does not run anything. Needs bash, sha256sum or shasum, awk and
# (for verify_release_assets) cosign.
#
# The smoke tests never use a secret in argv or in their output: the GitHub
# token stays in the environment (GH_TOKEN), the throw-away Hub admin password
# goes through a file (MINION_ADMIN_PASSWORD_FILE).

SMOKE_REPO="${GITHUB_REPOSITORY:-patrick-1984/ai-minion}"
SMOKE_IMAGE="${SMOKE_IMAGE:-ghcr.io/patrick-1984/minion}"
# The release identity: keep in step with SIGNER_IDENTITY in
# .github/workflows/release.yml and internal/version/identity.go.
SMOKE_OIDC_ISSUER="https://token.actions.githubusercontent.com"
SMOKE_SIGNER_WORKFLOW="https://github.com/${SMOKE_REPO}/.github/workflows/release.yml"

smoke_log() { printf 'smoke: %s\n' "$*" >&2; }
smoke_die() {
	smoke_log "FAIL: $*"
	exit 1
}

# smoke_valid_tag TAG: vX.Y.Z or vX.Y.Z-rc.N (the tags release.yml builds).
smoke_valid_tag() {
	printf '%s\n' "${1:-}" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$'
}

# smoke_require_tag TAG: exits unless TAG is a release tag.
smoke_require_tag() {
	smoke_valid_tag "${1:-}" || smoke_die "'${1:-}' is not a release tag (vX.Y.Z or vX.Y.Z-rc.N)"
}

# smoke_version TAG: the version without the leading v.
smoke_version() { printf '%s\n' "${1#v}"; }

# smoke_sha256 FILE: the sha256 of a file (Linux and macOS).
smoke_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# smoke_one_file DIR GLOB: prints the single file DIR/GLOB, exits when there is
# none or more than one. GitHub may rename an asset ("~" becomes "."), so the
# package files are found by pattern and checked by hash, never by name.
smoke_one_file() {
	local dir=$1 glob=$2 f found="" n=0
	for f in "$dir"/$glob; do
		[ -f "$f" ] || continue
		found=$f
		n=$((n + 1))
	done
	[ "$n" -eq 1 ] || smoke_die "expected exactly one '$glob' in $dir, found $n"
	printf '%s\n' "$found"
}

# smoke_archive_name TAG OS ARCH: the release archive name, as goreleaser names
# it (arm is "armv7"; Windows is a preview zip).
smoke_archive_name() {
	local ver os=$2 arch=$3
	ver=$(smoke_version "$1")
	case "$os" in
	windows) printf 'minion_%s_windows_%s_preview.zip\n' "$ver" "$arch" ;;
	*) printf 'minion_%s_%s_%s.tar.gz\n' "$ver" "$os" "$arch" ;;
	esac
}

# smoke_listed_in_checksums CHECKSUMS FILE: succeeds when the sha256 of FILE is
# a line of the (signed) checksums.txt. The hash is compared, not the name.
smoke_listed_in_checksums() {
	local sum
	sum=$(smoke_sha256 "$2")
	awk -v s="$sum" '$1 == s { found = 1 } END { exit !found }' "$1"
}

# smoke_verify_blob FILE TAG: verifies FILE against FILE.sigstore.json with the
# exact identity of the release workflow at refs/tags/TAG (AD-24), which also
# proves the Rekor inclusion (cosign verifies it by default).
smoke_verify_blob() {
	local file=$1 tag=$2
	[ -f "$file" ] || smoke_die "missing $file"
	[ -f "$file.sigstore.json" ] || smoke_die "missing the cosign bundle $file.sigstore.json"
	cosign verify-blob \
		--bundle "$file.sigstore.json" \
		--certificate-identity "${SMOKE_SIGNER_WORKFLOW}@refs/tags/${tag}" \
		--certificate-oidc-issuer "$SMOKE_OIDC_ISSUER" \
		--certificate-github-workflow-repository "$SMOKE_REPO" \
		--certificate-github-workflow-ref "refs/tags/${tag}" \
		"$file" >/dev/null || smoke_die "the cosign bundle of $(basename "$file") does not verify for $tag"
}

# smoke_verify_release_assets DIR TAG FILE...: DIR holds the downloaded assets
# of release TAG. Verifies the cosign bundle of checksums.txt, then that every
# FILE (names relative to DIR) hashes to a line of that checksums.txt. The
# files signed on their own (bootstrap.sh, release-manifest.json) are verified
# against their own bundles when present.
smoke_verify_release_assets() {
	local dir=$1 tag=$2 f
	shift 2
	smoke_require_tag "$tag"
	smoke_verify_blob "$dir/checksums.txt" "$tag"
	for f in "$@"; do
		[ -f "$dir/$f" ] || smoke_die "missing asset $f"
		smoke_listed_in_checksums "$dir/checksums.txt" "$dir/$f" ||
			smoke_die "$f does not match the signed checksums.txt of $tag"
	done
	for f in bootstrap.sh release-manifest.json; do
		[ -f "$dir/$f" ] || continue
		smoke_verify_blob "$dir/$f" "$tag"
	done
	smoke_log "verified checksums.txt, its cosign bundle and $# asset(s) of $tag"
}

# smoke_check_pinned_checksums FILE PIN: FILE (a downloaded checksums.txt) must
# have exactly the sha256 PIN that publish.yml recorded when it resolved the
# draft. A release that was rebuilt in between (release.yml may rerun while the
# release is still a draft) is not the release that was tested (AD-24).
smoke_check_pinned_checksums() {
	local file=$1 pin=$2 got
	printf '%s\n' "$pin" | grep -Eq '^[0-9a-f]{64}$' ||
		smoke_die "the pinned checksums.txt sha256 is not 64 lowercase hex digits"
	[ -f "$file" ] || smoke_die "missing $file"
	got=$(smoke_sha256 "$file")
	[ "$got" = "$pin" ] ||
		smoke_die "checksums.txt has sha256 $got, expected the pinned $pin: the draft changed after it was resolved"
}

# smoke_asset_manifest DIR: one "<sha256>  <name>" line for every file in DIR,
# sorted by name. This is the record of the complete asset set of a release
# (archives, packages, SBOMs, checksums.txt, release-manifest.json, bootstrap.sh
# and every cosign bundle): checksums.txt lists only part of it.
smoke_asset_manifest() {
	local dir=$1 f
	for f in "$dir"/*; do
		[ -f "$f" ] || continue
		printf '%s  %s\n' "$(smoke_sha256 "$f")" "$(basename "$f")"
	done | LC_ALL=C sort -k2,2
}

# smoke_require_signed_assets DIR: the files that carry the trust of a release
# are all there: checksums.txt and release-manifest.json with their cosign
# bundles, and a bundle for bootstrap.sh when there is a bootstrap.sh (and the
# other way round). A release without them is not a release.
smoke_require_signed_assets() {
	local dir=$1 f
	for f in checksums.txt release-manifest.json; do
		[ -f "$dir/$f" ] || smoke_die "the release has no $f"
		[ -f "$dir/$f.sigstore.json" ] || smoke_die "the release has no cosign bundle $f.sigstore.json"
	done
	if [ -f "$dir/bootstrap.sh" ] || [ -f "$dir/bootstrap.sh.sigstore.json" ]; then
		[ -f "$dir/bootstrap.sh" ] || smoke_die "the release has a bootstrap.sh.sigstore.json but no bootstrap.sh"
		[ -f "$dir/bootstrap.sh.sigstore.json" ] || smoke_die "the release has no cosign bundle bootstrap.sh.sigstore.json"
	fi
}

# smoke_check_asset_manifest DIR MANIFEST_FILE: the files in DIR are exactly the
# recorded asset set: no file missing, none added, none with other bytes.
smoke_check_asset_manifest() {
	local dir=$1 manifest=$2 got
	[ -s "$manifest" ] || smoke_die "the recorded asset manifest $manifest is missing or empty"
	got=$(mktemp)
	smoke_asset_manifest "$dir" >"$got"
	if ! diff -u "$manifest" "$got" >&2; then
		rm -f "$got"
		smoke_die "the release assets are not the tested ones (missing, added or changed files, see the diff above): the draft changed after it was resolved"
	fi
	rm -f "$got"
}

# smoke_asset_names_from_manifest MANIFEST_FILE: the asset names, one per line.
smoke_asset_names_from_manifest() { awk '{print $2}' "$1"; }

# smoke_verify_all_assets DIR TAG: DIR holds the complete assets of release TAG.
# Verifies the signed files with their bundles and that every other file is
# listed in the signed checksums.txt.
smoke_verify_all_assets() {
	local dir=$1 tag=$2 f b
	local files=()
	smoke_require_signed_assets "$dir"
	for f in "$dir"/*; do
		[ -f "$f" ] || continue
		b=$(basename "$f")
		case "$b" in
		checksums.txt | *.sigstore.json | bootstrap.sh | release-manifest.json) continue ;;
		esac
		files+=("$b")
	done
	smoke_verify_release_assets "$dir" "$tag" ${files[@]+"${files[@]}"}
}

# smoke_expect_version OUTPUT TAG: OUTPUT is "minion version" output; the
# version line must be exactly vX.Y.Z of TAG.
smoke_expect_version() {
	printf '%s\n' "$1" | grep -Fxq "version: $2" ||
		smoke_die "minion version printed '$(printf '%s\n' "$1" | head -n 1)', expected 'version: $2'"
}

# smoke_wait_for TIMEOUT_SECONDS DESCRIPTION COMMAND...: polls COMMAND once a
# second until it succeeds. COMMAND runs with stdout and stderr sent to
# /dev/null, so it must only report success or failure: a smoke_die (or any
# exit) inside it ends the script without a word. A poll that has a reason to
# stop early (the process it waits for died) must not use this function; it
# polls in the script itself, where the reason is printed (macos.sh does).
smoke_wait_for() {
	local timeout=$1 what=$2 i=0
	shift 2
	while ! "$@" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -le "$timeout" ] || smoke_die "timed out after ${timeout}s waiting for $what"
		sleep 1
	done
}

# smoke_timeout SECONDS COMMAND...: runs COMMAND (an external program) with a
# limit, for diagnostics that must never hang a script (a stopped tmux server
# makes every tmux client wait forever). The whole process tree of COMMAND is
# ended, not only its first process: COMMAND runs as the leader of a process
# group of its own (perl's setpgid, then exec: perl is on every GitHub Linux
# and macOS runner and macOS has no setsid or timeout program), and the group
# and only that group is signalled, TERM and, one second later, KILL. The group
# is created before COMMAND starts and a pid that leads a live group cannot be
# reused, so the signal cannot reach an unrelated process, the caller's process
# group or a tmux server of the caller (it is not a member). When there is no
# perl, or the group cannot be created, COMMAND is not run (fail closed, status
# 125, a message on stdout): there is no fallback without isolation.
#
# stdout and stderr of COMMAND are merged and printed after it ended. They go
# through a file, not a pipe, so that no descendant can hold a command
# substitution open; stdin is /dev/null. The exit status is that of COMMAND,
# 124 when the limit stopped it. Everything is bounded: at most 2 seconds to
# start, SECONDS to run, about 1 second to end the group; no cleanup waits
# for anything else, and nothing scans the process table (one kill -0 on the
# group per 0.1 second). A descendant that outlives COMMAND is ended too.
smoke_timeout() {
	local secs=$1 out ready pid rc=0 ticks=0 limit timed_out=0
	shift
	if ! command -v perl >/dev/null 2>&1; then
		printf '(diagnostic skipped: perl is not available to isolate its process group)\n'
		return 125
	fi
	out=$(mktemp) || return 125
	ready="$out.ready"
	perl -e 'use POSIX (); defined(POSIX::setpgid(0, 0)) or exit 125; open(my $f, ">", shift) or exit 125; close($f); exec { $ARGV[0] } @ARGV; print STDERR "cannot run $ARGV[0]: $!\n"; exit 127' \
		"$ready" "$@" >"$out" 2>&1 </dev/null &
	pid=$!
	# Wait for the group (the helper writes $ready after setpgid), at most 2 s.
	while [ ! -e "$ready" ]; do
		if ! kill -0 "$pid" 2>/dev/null; then
			break # the helper ended before it could start COMMAND
		fi
		ticks=$((ticks + 1))
		if [ "$ticks" -gt 40 ]; then
			kill -KILL "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
			rm -f "$out" "$ready"
			printf '(diagnostic skipped: the process group was not created in time)\n'
			return 125
		fi
		sleep 0.05
	done
	rm -f "$ready"
	# The group id is the pid of the helper (now COMMAND).
	limit=$((secs * 10))
	ticks=0
	while kill -0 "$pid" 2>/dev/null; do
		ticks=$((ticks + 1))
		if [ "$ticks" -gt "$limit" ]; then
			timed_out=1
			break
		fi
		sleep 0.1
	done
	# Whatever is left of the group (after a timeout, or a descendant that
	# outlived COMMAND) is ended: TERM, at most 1 second of grace, KILL.
	if kill -TERM -- "-$pid" 2>/dev/null; then
		ticks=0
		while kill -0 -- "-$pid" 2>/dev/null && [ "$ticks" -lt 10 ]; do
			ticks=$((ticks + 1))
			sleep 0.1
		done
		kill -KILL -- "-$pid" 2>/dev/null || true
	fi
	wait "$pid" 2>/dev/null || rc=$?
	if [ "$timed_out" -eq 1 ]; then
		rc=124
	fi
	cat "$out"
	rm -f "$out"
	return "$rc"
}

# smoke_redact: filters stdin to stdout and hides what looks like a secret, so
# that a log may be quoted in a failure report. Hidden: a bearer credential
# (first, so that no later rule can eat the word "Bearer" and leave the
# credential behind, however short it is) and the value of a key that is named
# like a secret (password, secret, token, key, credential, authorization,
# invite, pairing; as key=value, key: value or "key":"value"), a pairing code
# and any long run of base64 or hex characters.
#
# The rules run in this order so that none of them sees what an earlier one
# wrote: a value that starts with an escaped quote, an unquoted value, a
# quoted value, then a quoted value that is never closed.
#
# A quoted value ends at the first quote that is not escaped: a backslash
# escapes the next character, so \" and \\" (a backslash, then the closing
# quote) are told apart, and a value with escaped quotes is hidden whole. A
# value that starts with an escaped quote (JSON inside a quoted log field) is
# hidden to the end of the line. When a quote is never closed (the line ends or
# is cut inside the value), the rest of the line is hidden, and so is every
# later line: the value may go on there (conservative; one notice line takes
# their place).
#
# It runs on sed alone, with explicit character classes (BSD sed has no
# case-insensitive flag), and works on whole lines: always redact first and
# truncate after, never the other way round. It is a safety net, not a
# promise: the Worker log carries no secret (AD-20) and this is the second
# line of defence.
smoke_redact() {
	# mark: STX, a control character that is no special character of bash word
	# expansion (SOH, \001, is the CTLESC byte of bash, which older versions
	# such as the 3.2 of macOS handle badly).
	local kw="" w c i line seen=0 mark=$'\002' p dq sq auth
	for w in password passwd secret token key credential authorization invite pairing; do
		for ((i = 0; i < ${#w}; i++)); do
			c=${w:i:1}
			kw+="[$c$(printf '%s' "$c" | tr '[:lower:]' '[:upper:]')]"
		done
		kw+="|"
	done
	kw=${kw%|}
	# p: a secret-named key, its separator and the spaces after it (the key may
	# be quoted, also with escaped quotes). dq and sq: a quoted value up to, not
	# including, its closing quote: any character but the quote and the
	# backslash, or a backslash and the next character, or a lone backslash that
	# ends the line.
	p="(($kw)[A-Za-z0-9_-]*\\\\*[\"']?[[:space:]]*[=:][[:space:]]*)"
	# auth: the key of an HTTP credential (Authorization, Proxy-Authorization).
	auth='[Aa][Uu][Tt][Hh][Oo][Rr][Ii][Zz][Aa][Tt][Ii][Oo][Nn]["'"'"']?[[:space:]]*[=:][[:space:]]*'
	dq='"([^"\\]|\\.|\\$)*'
	sq="'([^'\\\\]|\\\\.|\\\\\$)*"
	tr -d '\002' | sed -E \
		-e "s/([Bb][Ee][Aa][Rr][Ee][Rr][[:space:]]+)(${dq}(\"|\$)|${sq}('|\$)|[^[:space:]]+)/\\1[redacted]/g" \
		-e "s/(${auth})[A-Za-z][A-Za-z0-9._-]*[[:space:]]+[^[:space:]]+([[:space:]]*,[[:space:]]*[A-Za-z][A-Za-z0-9_-]*=(\"[^\"]*\"|[^[:space:],]+))*/\\1[redacted]/g" \
		-e "s/${p}\\\\+\".*\$/\\1[redacted]/g" \
		-e "s/${p}[^[:space:],;\"']+[\"'].*\$/\\1[redacted]/g" \
		-e "s/${p}[^[:space:],;\"']+/\\1[redacted]/g" \
		-e "s/${p}${dq}\"/\\1[redacted]/g" \
		-e "s/${p}${sq}'/\\1[redacted]/g" \
		-e "s/${p}${dq}\$/\\1[redacted]${mark}/" \
		-e "s/${p}${sq}\$/\\1[redacted]${mark}/" \
		-e 's/([Pp][Aa][Ii][Rr][Ii][Nn][Gg][ _-]?[Cc][Oo][Dd][Ee][[:space:]]*[=:][[:space:]]*)[^[:space:]]+/\1[redacted]/g' \
		-e 's/[A-Za-z0-9+\/_=-]{32,}/[redacted]/g' |
		while IFS= read -r line || [ -n "$line" ]; do
			if [ "$seen" -eq 1 ]; then
				continue
			fi
			case $line in
			*"$mark"*)
				printf '%s\n' "${line//$mark/}"
				printf '[redacted: the lines after an unterminated quoted secret]\n'
				seen=1
				;;
			*) printf '%s\n' "$line" ;;
			esac
		done
}

# smoke_make_secret_file PATH: a throw-away random password in a 0644 file (it
# is read by another uid inside a container), never printed.
smoke_make_secret_file() {
	(umask 022 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$1")
	chmod 0644 "$1"
}
