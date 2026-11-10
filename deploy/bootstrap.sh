#!/bin/sh
# bootstrap.sh: the no-registry installer of Minion (Story 9.3, AD-24, AD-19).
#
# Any ordinary base image plus this script runs the Hub or a Worker without a
# registry pull: the script fetches the right release archive, verifies it,
# keeps it in an immutable cache and then runs
#
#     exec minion "$@"
#
# Usage (the arguments are passed to minion unchanged):
#
#     MINION_VERSION=1.4 sh bootstrap.sh hub serve
#     MINION_VERSION=v1.4.2 MINION_SHA256=<archive sha256> sh bootstrap.sh worker run
#
# Environment (all optional; an empty value counts as unset):
#
#   MINION_VERSION    What to run: an exact version (v1.4.2, 1.4.2), a channel
#                     (1.4 = newest 1.4.x, 1 = newest 1.x.y, latest = newest
#                     release) or nothing (= latest). A release candidate
#                     (-rc.N) can only be requested by its exact version; it
#                     never satisfies a channel.
#   MINION_SHA256     sha256 of the archive for the exact version (the line in
#                     that release's checksums.txt). Allowed only with an exact
#                     version, because a channel moves; with a channel the
#                     script stops and says so. With it the script needs no
#                     cosign and no checksums.txt.
#   MINION_CACHE_DIR  Cache directory, keep it on a volume. Default:
#                     /var/cache/minion for root, else
#                     ${XDG_CACHE_HOME:-$HOME/.cache}/minion.
#   MINION_RELEASE_BASE, MINION_RELEASE_API, MINION_COSIGN_BASE
#                     Where releases, the release list and the cosign binary
#                     are fetched from (mirrors, tests). They change only where
#                     bytes come from, never what is trusted: every archive is
#                     verified against MINION_SHA256 or the signed
#                     checksums.txt, and cosign against the sha256 pinned below.
#
# Verification (AD-24):
#   * exact version + MINION_SHA256: the archive must hash to MINION_SHA256.
#   * everything else: checksums.txt and its cosign bundle are verified with the
#     pinned cosign (a sha256 per architecture is embedded below) against the
#     same identity constants as "minion update" (internal/version, Story 9.2):
#     the release workflow at refs/tags/<resolved version>, issued by GitHub
#     Actions, built from this repository at that tag (the source repository and
#     source ref claims of the certificate, as in the updater), with Rekor inclusion (cosign verifies the log entry in the
#     bundle by default); then the archive must match checksums.txt.
#
# Cache: <cache>/<version>-<archive sha256>/ holds the verified release archive,
# its proof and the architecture it was built for (other architectures never
# use it). An entry is created once (built in a temporary directory, published
# by one rename) and never changed. The proof of an entry made with
# MINION_SHA256 is that pin; the proof of an entry made by signature is the
# signed checksums.txt, its cosign bundle and the Sigstore trusted root that
# verified it. A cache entry is never trusted for its name, nor for a digest
# file beside a binary: on EVERY start the archive is copied into a private
# staging directory, proven there (the archive must hash to the explicit pin, or
# the signed checksums.txt must verify offline with the stored trusted root and
# list that archive), and the binary that runs is extracted from that proven
# copy. An entry made under a pin is used only when the same pin is given again;
# an unpinned start uses only entries made by signature. A start without
# network, or with a release host that cannot be reached, runs the newest such
# cached version that matches the selector, or fails with a clear message.
# Channels are re-resolved only while the network is available. A failed
# verification never falls back to the cache. Caches written by earlier versions
# of this script hold an unauthenticated binary and are not reused.
#
# Who may write the cache: the code in it is executed, so the cache is trusted
# only when it is owned by the user that runs this script or by root and is not
# writable by group or others (a group can have other members, and there is no
# list of trusted writers, so the user's own group does not count as trusted),
# and every directory and symlink on the way to it, including the hops of a
# symlink chain, has the same property (a sticky directory like /tmp is allowed
# above the cache: it lets a user rename only their own entries). The cache
# entries, the run directory and the files used must be plain files and
# directories with the same owner and modes, and no symlink may be owned by
# another user. Anything else stops the start with a message. The script never
# changes the ownership or modes of an existing cache for you. This protects a
# launcher with more privilege than the writers of the cache from a planted
# binary; it does not isolate one process of the same user from another,
# because they already share the user's authority.
#
# Publishing an entry takes a lock, the directory <cache>/.lock.<entry>. The
# wait is bounded (MINION_LOCK_WAIT seconds, default 60); a lock that stays is
# never removed or taken over, the start fails and names it. It is removed on
# exit and on a signal.
#
# Outbound connections (AD-19; open exactly these for a firewalled install):
#   * api.github.com                  the release list (channels only);
#   * github.com release downloads    the Minion archive, checksums.txt and its
#     bundle, and the pinned sigstore/cosign binary (GitHub redirects the
#     downloads to its release-asset hosts, so allow those redirects too);
#   * tuf-repo-cdn.sigstore.dev       used by cosign itself, to fetch the Sigstore
#     trusted root that verifies the bundle (not needed with MINION_SHA256).
# With MINION_SHA256 and a cached or downloaded exact version only the archive
# download is needed. The script contacts nothing else. It never puts a secret
# in argv or in its output. Needs: sh, curl or wget, tar, gzip, awk, sed, grep,
# sha256sum (or shasum), uname, mktemp, readlink, rmdir, id, ls, cat, cp, mv.

set -eu
umask 022

# ---------------------------------------------------------------- constants
# The release identity: keep in step with internal/version/identity.go
# (Story 9.2) and SIGNER_IDENTITY in .github/workflows/release.yml.
REPO="patrick-1984/ai-minion"
SIGNER_WORKFLOW="https://github.com/${REPO}/.github/workflows/release.yml"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

# Pinned cosign (the version the release workflow signs with) and its sha256
# for each architecture, from cosign_checksums.txt of that release.
COSIGN_VERSION="v3.1.4"
# Where the pinned cosign keeps, below its TUF root, the trusted root it fetched from
# the default (production) Sigstore mirror.
TUF_PROD_MIRROR="tuf-repo-cdn.sigstore.dev"
COSIGN_SHA256_AMD64="424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa"
COSIGN_SHA256_ARM64="ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2"
COSIGN_SHA256_ARMV7="09e77a313910cf5c4774508d85d017d9c60983f41bf657f83ecbd3892f7952cb"

# Version number grammar: no leading zeros, at most six digits per part.
N='(0|[1-9][0-9]{0,5})'
RE_STABLE="^v${N}\\.${N}\\.${N}\$"
RE_RC="^v${N}\\.${N}\\.${N}-rc\\.[1-9][0-9]{0,5}\$"

# ------------------------------------------------------------------ helpers
log() { printf 'bootstrap: %s\n' "$*" >&2; }
die() {
	printf 'bootstrap: error: %s\n' "$*" >&2
	exit 1
}

TMP=""
LOCK=""
cleanup() {
	if [ -n "$LOCK" ]; then rmdir "$LOCK" 2>/dev/null || true; LOCK=""; fi
	if [ -n "$TMP" ] && [ -d "$TMP" ]; then rm -rf "$TMP"; fi
}
EUID_NOW=$(id -u)
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

need_downloader() {
	command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
		die "neither curl nor wget is installed (needed to download the release)"
}

# fetch URL DEST: 0 on success, 1 when the bytes could not be fetched.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --connect-timeout 10 --max-time 600 --retry 2 --proto-redir '=https' -o "$2" "$1" 2>/dev/null
	elif command -v wget >/dev/null 2>&1; then
		# -t 2: GNU wget tries 20 times by default, which with -T 60 would block an
		# offline start for ~20 minutes before the cache fallback (busybox ignores -t).
		wget -q -T 60 -t 2 -O "$2" "$1" 2>/dev/null
	else
		return 1
	fi
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "neither sha256sum nor shasum is installed"
	fi
}

# vkey VERSION: a fixed-width sort key, so that v1.4.10 sorts above v1.4.2.
vkey() {
	printf '%s\n' "$1" | sed 's/^v//; s/-.*//' |
		awk -F. '{ printf "%09d%09d%09d\n", $1, $2, $3 }'
}

is_stable() { printf '%s\n' "$1" | grep -Eq "$RE_STABLE"; }
is_exact() { is_stable "$1" || printf '%s\n' "$1" | grep -Eq "$RE_RC"; }

# in_selector VERSION: is the stable VERSION covered by the channel selector?
in_selector() {
	is_stable "$1" || return 1
	case "$SEL_KIND" in
	latest) return 0 ;;
	major) case "$1" in "v$SEL_PREFIX".*) return 0 ;; esac ;;
	minor) case "$1" in "v$SEL_PREFIX".*) return 0 ;; esac ;;
	esac
	return 1
}

# ---------------------------------------------------------------- selector
parse_selector() {
	sel="${MINION_VERSION:-latest}"
	sel="${sel#v}"
	SEL_KIND="" SEL_PREFIX="" SEL_VERSION=""
	if [ "$sel" = latest ]; then
		SEL_KIND=latest
	elif printf '%s\n' "$sel" | grep -Eq "^${N}\$"; then
		SEL_KIND=major SEL_PREFIX="$sel"
	elif printf '%s\n' "$sel" | grep -Eq "^${N}\\.${N}\$"; then
		SEL_KIND=minor SEL_PREFIX="$sel"
	elif is_exact "v$sel"; then
		SEL_KIND=exact SEL_VERSION="v$sel"
	else
		die "invalid MINION_VERSION '${MINION_VERSION}': use an exact version (v1.4.2), a channel (1.4, 1, latest) or leave it unset"
	fi

	SHA=""
	if [ -n "${MINION_SHA256:-}" ]; then
		if [ "$SEL_KIND" != exact ]; then
			die "MINION_SHA256 is allowed only with an exact MINION_VERSION such as v1.4.2: a channel ('${MINION_VERSION:-latest}') moves to new releases, so one checksum cannot describe it. Pin an exact version, or drop MINION_SHA256 to verify the channel by signature"
		fi
		SHA=$(printf '%s' "$MINION_SHA256" | tr 'A-F' 'a-f')
		printf '%s\n' "$SHA" | grep -Eq '^[0-9a-f]{64}$' ||
			die "MINION_SHA256 must be the 64 hex digits of the archive sha256"
	fi
}

detect_arch() {
	os=$(uname -s)
	[ "$os" = Linux ] || die "unsupported operating system '$os': bootstrap.sh runs on Linux (install the macOS or Windows release instead)"
	m=$(uname -m)
	case "$m" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	armv7l | armv7 | armv8l) ARCH=armv7 ;;
	*) die "unsupported architecture '$m' (supported: amd64, arm64, armv7)" ;;
	esac
}

# ------------------------------------------------------------- cache trust
# The cache is code that gets executed, so who can write it matters. "ls -ldn"
# (the same on GNU and busybox) gives the type, the mode and the numeric owner;
# stat is not assumed.

# node_info PATH: sets NODE_MODE, NODE_TYPE (d, l, f or o) and NODE_UID.
node_info() {
	_ni=$(ls -ldn -- "$1" 2>/dev/null) || {
		NODE_WHY="cannot be inspected"
		return 1
	}
	# shellcheck disable=SC2086
	set -- $_ni
	NODE_MODE=${1:-} NODE_UID=${3:-}
	case $NODE_MODE in
	d*) NODE_TYPE=d ;;
	l*) NODE_TYPE=l ;;
	-*) NODE_TYPE=f ;;
	*) NODE_TYPE=o ;;
	esac
	case $NODE_MODE in ??????????*) ;; *)
		NODE_WHY="cannot be inspected"
		return 1
		;;
	esac
	return 0
}

# owner_trusted: is NODE_UID root or the user that runs this script?
owner_trusted() {
	if [ "$NODE_UID" = 0 ] || [ "$NODE_UID" = "$EUID_NOW" ]; then return 0; fi
	NODE_WHY="is owned by uid $NODE_UID (not by root or by uid $EUID_NOW)"
	return 1
}

# trusted_node PATH dir|file [sticky_ok]: PATH is that kind of object (a symlink
# is neither), owned by root or this user, and not writable by group or others.
# A group, even the primary group of this user or root's, can have other members
# and there is no list of trusted writers, so group write is refused like world
# write. A sticky directory may be world-writable when sticky_ok (like /tmp): it
# lets a user remove or rename only their own entries, and every entry on the
# way is checked for its owner as well.
trusted_node() {
	node_info "$1" || return 1
	case "$2:$NODE_TYPE" in
	dir:d | file:f) ;;
	*)
		NODE_WHY="is not a plain $2 (it is a symlink or another kind of file)"
		return 1
		;;
	esac
	owner_trusted || return 1
	case $NODE_MODE in
	?????w????* | ????????w?*)
		if [ "${3:-}" = sticky_ok ]; then
			case $NODE_MODE in ?????????[tT]*) return 0 ;; esac
		fi
		NODE_WHY="is writable by group or others"
		return 1
		;;
	esac
	return 0
}

# walk_trusted PATH: PATH is absolute and normalised (no trailing slash). Resolve
# it one component at a time, like the kernel does, and check every directory
# and every symlink met on the way, including the directories a symlink points
# through (an intermediate hop in a world-writable directory would let another
# user redirect the path although its start and its end are safe). A directory
# must be trusted (see trusted_node; sticky ones only above the final directory)
# and a symlink must be owned by root or by this user. On success WALKED is the
# resolved physical path. On failure TRUST_AT is the object that is not trusted
# and NODE_WHY says why. Needs readlink (GNU, busybox and macOS have it).
walk_trusted() {
	_cur=/
	_hops=0
	_rest=${1#/}
	TRUST_AT=/
	trusted_node / dir sticky_ok || return 1
	while [ -n "$_rest" ]; do
		_comp=${_rest%%/*}
		case $_rest in */*) _rest=${_rest#*/} ;; *) _rest="" ;; esac
		case $_comp in
		"" | .) continue ;;
		..)
			_cur=${_cur%/*}
			[ -n "$_cur" ] || _cur=/
			continue
			;;
		esac
		_next=${_cur%/}/$_comp
		TRUST_AT=$_next
		if [ -L "$_next" ]; then
			if ! node_info "$_next" || ! owner_trusted; then return 1; fi
			_hops=$((_hops + 1))
			if [ "$_hops" -gt 40 ]; then
				NODE_WHY="is part of a symlink chain that is too long or loops"
				return 1
			fi
			_tgt=$(readlink "$_next" 2>/dev/null) && [ -n "$_tgt" ] || {
				NODE_WHY="is a symlink that cannot be read (readlink is needed)"
				return 1
			}
			case $_tgt in /*) ;; *) _tgt=${_cur%/}/$_tgt ;; esac
			# Resolve what the link points to from the root: _cur is physical, so the
			# rest of the path is a fresh walk over the target and what remains.
			_rest=${_tgt#/}${_rest:+/$_rest}
			_cur=/
			continue
		fi
		if [ -n "$_rest" ]; then
			trusted_node "$_next" dir sticky_ok || return 1
		else
			trusted_node "$_next" dir || return 1
		fi
		_cur=$_next
	done
	WALKED=$_cur
	return 0
}

untrusted() { # WHAT PATH
	die "$1 '$2' is not trusted: it ${NODE_WHY}. bootstrap.sh runs code from this cache, so the cache and everything above it must belong to root or to the user that runs the script and must not be writable by anyone else (this includes the group: a group can have other members), and bootstrap will not change it for you. Fix the ownership and modes (chown, chmod 755), or use another MINION_CACHE_DIR. If the cache may have been written by another user, delete it and start again"
}

# check_dir / check_file WHAT PATH: die unless trusted.
check_dir() { trusted_node "$2" dir || untrusted "$1" "$2"; }
check_file() { trusted_node "$2" file || untrusted "$1" "$2"; }

# -------------------------------------------------------------------- cache
setup_cache() {
	if [ -n "${MINION_CACHE_DIR:-}" ]; then
		CACHE="$MINION_CACHE_DIR"
	elif [ "$EUID_NOW" = 0 ]; then
		CACHE=/var/cache/minion
	else
		CACHE="${XDG_CACHE_HOME:-${HOME:-/tmp}/.cache}/minion"
	fi
	case $CACHE in /*) ;; *) CACHE="$(pwd)/$CACHE" ;; esac
	while [ "${CACHE%/}" != "$CACHE" ] && [ "$CACHE" != / ]; do CACHE=${CACHE%/}; done
	mkdir -p "$CACHE" 2>/dev/null && [ -w "$CACHE" ] ||
		die "the cache directory '$CACHE' is not writable: mount a writable volume there or set MINION_CACHE_DIR"
	logical=$CACHE
	CACHE=$(cd -P "$CACHE" 2>/dev/null && pwd -P) || die "cannot resolve the cache directory '$logical'"
	# Every directory and symlink on the path as given, and on the resolved path, must
	# be trusted: see walk_trusted. From here on only the resolved path is used.
	for _path in "$logical" "$CACHE"; do
		walk_trusted "$_path" || {
			if [ "$TRUST_AT" = "$CACHE" ] || [ "$TRUST_AT" = "$logical" ]; then untrusted "the cache directory" "$TRUST_AT"; fi
			untrusted "an ancestor or link of the cache directory (the object" "$TRUST_AT)"
		}
		[ "$WALKED" = "$CACHE" ] || die "the cache directory '$logical' changed while it was checked; start again"
	done
}

# is_shaped DIR: the entry has an archive for this architecture, its architecture
# record and the completion marker (a pure shape test: nothing is trusted yet).
is_shaped() {
	_d=$1
	_n=${_d##*/}
	_v=${_n%-*}
	[ -d "$_d" ] && [ -f "$_d/.complete" ] && [ -f "$_d/minion_${_v#v}_linux_${ARCH}.tar.gz" ] || return 1
	[ "$(cat "$_d/arch" 2>/dev/null || true)" = "$ARCH" ]
}

# is_signed_entry DIR: the entry carries the proof of a signature install.
is_signed_entry() {
	[ -f "$1/checksums.txt" ] && [ -f "$1/checksums.txt.sigstore.json" ] && [ -f "$1/trusted_root.json" ]
}

# cached_entries: "<sortkey> <version> <dir>" for every complete entry built
# for this architecture (the entry records it in the file "arch": a cache that
# was moved to another architecture must not run the foreign binary). Under a
# pin only an entry named for that pin qualifies; without one only an entry made
# by signature does. Nothing is proven here: run_entry does that.
cached_entries() {
	for d in "$CACHE"/v[0-9]*-*; do
		is_shaped "$d" || continue
		name=${d##*/}
		ver=${name%-*}
		is_exact "$ver" || continue
		if [ -n "$SHA" ]; then
			[ "${name##*-}" = "$SHA" ] || continue
		else
			is_signed_entry "$d" || continue
		fi
		printf '%s %s %s\n' "$(vkey "$ver")" "$ver" "$d"
	done
}

# find_cached_exact VERSION: the entry directory, honouring MINION_SHA256.
find_cached_exact() {
	cached_entries | while read -r _ ver dir; do
		[ "$ver" = "$1" ] || continue
		printf '%s\n' "$dir"
		break
	done
}

# find_cached_channel: the newest stable cached entry of the channel.
find_cached_channel() {
	cached_entries | sort -r | while read -r _ ver dir; do
		in_selector "$ver" || continue
		printf '%s\n' "$dir"
		break
	done
}

# copy_trusted FILE DEST: FILE must be a trusted plain file; DEST is a copy
# in the private staging directory.
copy_trusted() {
	check_file "the cache file" "$1"
	cat "$1" >"$2" 2>/dev/null || die "cannot read the cache file '$1'"
}

# stage_cosign: COSIGN is a private copy of the pinned cosign. The copy is what is
# hashed and what runs, so the cache cannot change it in between. 0 when it is
# there and has the pinned digest, 1 when the cached binary is missing or wrong.
stage_cosign() {
	case "$ARCH" in
	amd64) want=$COSIGN_SHA256_AMD64 ;;
	arm64) want=$COSIGN_SHA256_ARM64 ;;
	armv7) want=$COSIGN_SHA256_ARMV7 ;;
	esac
	cached="$CACHE/tools/cosign-$COSIGN_VERSION-$ARCH"
	[ -e "$cached" ] || return 1
	check_dir "the cache directory" "$CACHE/tools"
	check_file "the cached cosign" "$cached"
	COSIGN="$TMP/cosign-run"
	cat "$cached" >"$COSIGN" 2>/dev/null && chmod 700 "$COSIGN" || return 1
	[ "$(sha256_of "$COSIGN")" = "$want" ]
}

# cosign_verify VERSION BUNDLE BLOB TUF_DIR [TRUSTED_ROOT]: verify the signature
# of the checksums file against the release identity with the staged cosign.
# With TRUSTED_ROOT the Sigstore trusted root comes from that file and cosign
# needs no network; otherwise it fetches it through TUF into TUF_DIR. Every
# inherited variable that could change a trust root is dropped.
cosign_verify() {
	(
		TUF_ROOT="$4"
		export TUF_ROOT
		unset TUF_MIRROR TUF_ROOT_JSON SIGSTORE_ROOT_FILE SIGSTORE_REKOR_PUBLIC_KEY SIGSTORE_CT_LOG_PUBLIC_KEY_FILE SIGSTORE_TSA_CERTIFICATE_FILE
		_ver=$1 _bundle=$2 _blob=$3 _root=${5:-}
		set -- --bundle "$_bundle" \
			--certificate-identity "${SIGNER_WORKFLOW}@refs/tags/${_ver}" \
			--certificate-oidc-issuer "$OIDC_ISSUER" \
			--certificate-github-workflow-repository "$REPO" \
			--certificate-github-workflow-ref "refs/tags/${_ver}" \
			"$_blob"
		if [ -n "$_root" ]; then set -- --trusted-root "$_root" "$@"; fi
		exec "$COSIGN" verify-blob "$@"
	)
}

# authenticate_entry DIR: prove the cache entry from its archive and set RUN_BIN
# to the binary extracted from the proven copy. Dies when it cannot.
authenticate_entry() {
	dir=$1
	name=${dir##*/}
	ver=${name%-*}
	entry_digest=${name##*-}
	archive="minion_${ver#v}_linux_${ARCH}.tar.gz"
	check_dir "the cache entry" "$dir"
	st="$TMP/auth"
	mkdir "$st" || die "cannot create '$st'"
	copy_trusted "$dir/$archive" "$st/$archive"
	digest=$(sha256_of "$st/$archive")
	[ "$digest" = "$entry_digest" ] ||
		die "the cached archive '$dir/$archive' does not hash to $entry_digest, the digest its directory is named for (it hashes to $digest); the entry was damaged or forged. Delete '$dir' and start again"
	if [ -n "$SHA" ]; then
		[ "$digest" = "$SHA" ] ||
			die "the cached archive '$dir/$archive' does not hash to the MINION_SHA256 you gave ($SHA); it hashes to $digest. Delete '$dir' and start again"
	else
		is_signed_entry "$dir" ||
			die "the cache entry '$dir' was not made by signature verification and no MINION_SHA256 is given: pin it with MINION_SHA256 or delete it"
		for f in checksums.txt checksums.txt.sigstore.json trusted_root.json; do copy_trusted "$dir/$f" "$st/$f"; done
		# The pinned cosign comes from the cache (hashed in staging) or, when it is
		# missing or damaged, from the release host (hashed against the pin).
		ensure_cosign ||
			die "the pinned cosign ${COSIGN_VERSION} is missing or damaged in '$CACHE/tools' and could not be fetched, so the cached signature cannot be checked: connect to the release host once, or pin the archive with MINION_SHA256"
		mkdir "$st/tuf" || die "cannot create '$st/tuf'"
		cosign_verify "$ver" "$st/checksums.txt.sigstore.json" "$st/checksums.txt" "$st/tuf" "$st/trusted_root.json" \
			>/dev/null 2>"$st/cosign.err" || {
			sed 's/^/bootstrap: cosign: /' "$st/cosign.err" >&2
			die "the signature of the cached checksums.txt for $ver could not be verified (identity ${SIGNER_WORKFLOW}@refs/tags/${ver}); the entry '$dir' was damaged or forged. Delete it and start again"
		}
		want=$(awk -v n="$archive" '$2 == n || $2 == "*" n { print $1; exit }' "$st/checksums.txt")
		[ -n "$want" ] && [ "$digest" = "$want" ] ||
			die "the signed checksums.txt in '$dir' does not list the digest $digest for $archive; the entry was damaged or forged. Delete it and start again"
	fi
	mkdir "$st/x" || die "cannot create '$st/x'"
	extract_binary "$st/$archive" "$st/x"
	place_binary "$st/x/minion" "$name"
}

# extract_binary ARCHIVE DIR: DIR/minion from the archive, a plain executable file.
extract_binary() {
	tar -xzf "$1" -C "$2" minion 2>/dev/null || die "$1 does not contain the minion binary"
	[ -f "$2/minion" ] && [ ! -L "$2/minion" ] || die "$1 holds no regular file 'minion'"
	chmod 755 "$2/minion" || die "cannot make the minion binary executable"
}

# place_binary SRC NAME: move the binary that was extracted from a proven
# archive to <cache>/.run/<entry>/minion, a directory only its owner can write,
# and set RUN_BIN. It is moved by rename, so a running copy is never changed.
place_binary() {
	run_root="$CACHE/.run"
	run_dir="$run_root/$2"
	# A concurrent start may create the same directories: only the final state matters.
	(umask 077 && mkdir -p "$run_root" 2>/dev/null) || [ -d "$run_root" ] || die "cannot create '$run_root'"
	check_dir "the run directory" "$run_root"
	(umask 077 && mkdir "$run_dir" 2>/dev/null) || [ -d "$run_dir" ] || die "cannot create '$run_dir'"
	check_dir "the run directory" "$run_dir"
	bin_digest=$(sha256_of "$1")
	mv -f "$1" "$run_dir/minion" || die "cannot place the binary in '$run_dir'"
	RUN_BIN="$run_dir/minion"
	check_file "the binary" "$RUN_BIN"
	[ "$(sha256_of "$RUN_BIN")" = "$bin_digest" ] || die "the binary in '$run_dir' changed while it was placed"
}

# exec_binary ARGS...: replace this process by RUN_BIN. Never returns.
exec_binary() {
	cleanup
	TMP=""
	unset MINION_VERSION MINION_SHA256 MINION_CACHE_DIR MINION_LOCK_WAIT MINION_RELEASE_BASE MINION_RELEASE_API MINION_COSIGN_BASE
	exec "$RUN_BIN" "$@"
}

# run_entry DIR ARGS...: prove the entry, then replace this process by the
# binary extracted from its proven archive. Never returns.
run_entry() {
	dir=$1
	shift
	authenticate_entry "$dir"
	log "running ${dir##*/}"
	exec_binary "$@"
}

# ------------------------------------------------------------ release lookup
# resolve_online: print the newest stable release of the channel.
# 0 = found, 1 = the release list could not be fetched, 2 = nothing matches.
resolve_online() {
	api="${MINION_RELEASE_API:-https://api.github.com/repos/${REPO}/releases}"
	list="$TMP/releases.list"
	: >"$list"
	page=1
	while [ "$page" -le 10 ]; do
		fetch "${api}?per_page=100&page=${page}" "$TMP/releases.json" || {
			[ "$page" -gt 1 ] && break
			return 1
		}
		# GitHub pretty-prints; splitting on commas also handles minified JSON.
		tr ',' '\n' <"$TMP/releases.json" | awk '
			function flush() { if (have && !bad) print cur; have = 0 }
			/"tag_name"[ \t]*:/ {
				flush()
				s = $0
				sub(/.*"tag_name"[ \t]*:[ \t]*"/, "", s)
				sub(/".*/, "", s)
				cur = s; bad = 0; have = 1
			}
			/"prerelease"[ \t]*:[ \t]*true/ { bad = 1 }
			/"draft"[ \t]*:[ \t]*true/ { bad = 1 }
			END { flush() }' >>"$list"
		count=$(tr ',' '\n' <"$TMP/releases.json" | grep -c '"tag_name"' || true)
		[ "$count" -ge 100 ] || break
		page=$((page + 1))
	done
	[ -s "$list" ] || return 2
	best=$(while read -r ver; do
		in_selector "$ver" || continue
		printf '%s %s\n' "$(vkey "$ver")" "$ver"
	done <"$list" | sort -r | head -n 1 | awk '{print $2}')
	[ -n "$best" ] || return 2
	printf '%s\n' "$best"
}

# ------------------------------------------------------------------- cosign
# ensure_cosign: set COSIGN to a private copy of the pinned binary. 0, or 1 when it
# could not be fetched; a binary whose sha256 differs from the pinned one is fatal.
ensure_cosign() {
	case "$ARCH" in
	amd64) want=$COSIGN_SHA256_AMD64 asset=cosign-linux-amd64 ;;
	arm64) want=$COSIGN_SHA256_ARM64 asset=cosign-linux-arm64 ;;
	armv7) want=$COSIGN_SHA256_ARMV7 asset=cosign-linux-arm ;;
	esac
	mkdir -p "$CACHE/tools" || die "cannot create '$CACHE/tools'"
	check_dir "the cache directory" "$CACHE/tools"
	if stage_cosign; then return 0; fi
	base="${MINION_COSIGN_BASE:-https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}}"
	log "fetching cosign ${COSIGN_VERSION} (${ARCH})"
	fetch "$base/$asset" "$TMP/cosign" || return 1
	got=$(sha256_of "$TMP/cosign")
	[ "$got" = "$want" ] ||
		die "the downloaded cosign has sha256 $got, the pinned value is $want; refusing to run it"
	chmod 755 "$TMP/cosign" || die "cannot make the downloaded cosign executable"
	cp "$TMP/cosign" "$TMP/cosign-store" || die "cannot store cosign in '$CACHE/tools'"
	mv -f "$TMP/cosign-store" "$CACHE/tools/cosign-$COSIGN_VERSION-$ARCH" || die "cannot store cosign in '$CACHE/tools'"
	COSIGN="$TMP/cosign-run"
	mv -f "$TMP/cosign" "$COSIGN" || die "cannot stage cosign"
}

# ------------------------------------------------------------------ publication lock
# acquire_lock NAME: take the publication lock of the entry NAME, a directory
# created by mkdir (atomic on every file system, in the trusted cache). The check
# for a missing or incomplete entry, the move of debris and the publishing rename
# are one critical section, so another start cannot complete an entry between
# the check and the rename and then lose it. The wait is bounded
# (MINION_LOCK_WAIT seconds, default 60). A lock that is still there after that
# is never removed or taken over, because it cannot be told from a start that is
# about to publish: the start fails closed and says which directory to check.
# The lock is released on exit and on a signal.
acquire_lock() {
	_lock="$CACHE/.lock.$1"
	_wait=${MINION_LOCK_WAIT:-60}
	case $_wait in "" | *[!0-9]*) _wait=60 ;; esac
	_n=0
	while ! mkdir "$_lock" 2>/dev/null; do
		_n=$((_n + 1))
		if [ "$_n" -gt "$_wait" ]; then
			die "another start has held '$_lock' for $_wait seconds while publishing $1; nothing was changed. If no bootstrap.sh is running (a start that was killed leaves it behind), remove that directory with rmdir and start again"
		fi
		[ "$_n" -ne 1 ] || log "waiting for another start to publish $1"
		sleep 1
	done
	LOCK=$_lock
}

release_lock() {
	if [ -n "$LOCK" ]; then rmdir "$LOCK" 2>/dev/null || true; fi
	LOCK=""
}

# -------------------------------------------------------------------- install
# install_version VERSION [EXPECTED_ARCHIVE_SHA256]
# Downloads, verifies and publishes one cache entry (the archive and its proof),
# and sets RUN_BIN to the binary extracted from the verified archive. Returns 1
# when a download failed (the caller may fall back for channels); a failed
# verification exits.
install_version() {
	ver=$1
	expect=${2:-}
	archive="minion_${ver#v}_linux_${ARCH}.tar.gz"
	rel="${MINION_RELEASE_BASE:-https://github.com/${REPO}/releases/download}/$ver"
	work="$TMP/work"
	mkdir -p "$work/x" || die "cannot create a work directory in '$TMP'"
	signed=""

	need_downloader
	log "downloading $archive ($ver)"
	fetch "$rel/$archive" "$work/$archive" || {
		log "could not download $rel/$archive"
		return 1
	}
	digest=$(sha256_of "$work/$archive")

	if [ -n "$expect" ]; then
		[ "$digest" = "$expect" ] ||
			die "checksum mismatch for $archive: MINION_SHA256 is $expect, the download hashes to $digest; nothing was installed"
	else
		fetch "$rel/checksums.txt" "$work/checksums.txt" || {
			log "could not download checksums.txt"
			return 1
		}
		fetch "$rel/checksums.txt.sigstore.json" "$work/checksums.txt.sigstore.json" || {
			log "could not download the signature bundle checksums.txt.sigstore.json"
			return 1
		}
		ensure_cosign || {
			log "could not download cosign"
			return 1
		}
		# cosign v3 needs a live TUF trusted root for a new-format bundle and fails
		# when $HOME/.sigstore cannot be created (HOME is / for a user without a
		# passwd entry, such as UID 65532). It gets a FRESH private TUF directory for
		# this verification, never one kept in the cache: cosign then starts from the
		# production trust root embedded in the pinned binary and the default mirror,
		# and nothing a cache holds (remote.json, root.json, targets) can choose the
		# trust anchor. Every inherited variable that could change its trust roots is
		# dropped too.
		tuf="$TMP/tuf-live"
		mkdir "$tuf" || die "cannot create '$tuf'"
		cosign_verify "$ver" "$work/checksums.txt.sigstore.json" "$work/checksums.txt" "$tuf" \
			>/dev/null 2>"$work/cosign.err" || {
			sed 's/^/bootstrap: cosign: /' "$work/cosign.err" >&2
			die "the signature of checksums.txt for $ver could not be verified (identity ${SIGNER_WORKFLOW}@refs/tags/${ver}, repository ${REPO}, issuer ${OIDC_ISSUER}); cosign also needs tuf-repo-cdn.sigstore.dev to fetch the Sigstore trusted root; nothing was installed"
		}
		want=$(awk -v n="$archive" '$2 == n || $2 == "*" n { print $1; exit }' "$work/checksums.txt")
		[ -n "$want" ] || die "checksums.txt of $ver does not list $archive"
		[ "$digest" = "$want" ] ||
			die "checksum mismatch for $archive: the signed checksums.txt says $want, the download hashes to $digest; nothing was installed"
		# The Sigstore trusted root that verified the release lets the cached proof be
		# checked again offline (cosign with --trusted-root needs no network). It is
		# the exact file of the live verification: the TUF directory is fresh, so the
		# only root in it is the one at the default (production) mirror's path, and
		# nothing else is picked. It is stored only after the bundle verifies against
		# this very copy offline, in another fresh directory.
		root="$tuf/$TUF_PROD_MIRROR/targets/trusted_root.json"
		nroots=$(find "$tuf" -name trusted_root.json 2>/dev/null | grep -c . || true)
		if [ "$nroots" = 1 ] && [ -f "$root" ] && [ ! -L "$root" ] &&
			cat "$root" >"$work/trusted_root.json" 2>/dev/null && [ -s "$work/trusted_root.json" ] &&
			mkdir "$work/tuf-offline" &&
			cosign_verify "$ver" "$work/checksums.txt.sigstore.json" "$work/checksums.txt" "$work/tuf-offline" "$work/trusted_root.json" \
				>/dev/null 2>"$work/cosign-offline.err"; then
			signed=1
		else
			rm -f "$work/trusted_root.json"
			log "warning: the Sigstore trusted root of this verification could not be captured and proven again offline: this entry can be used offline only with MINION_SHA256"
		fi
	fi

	# The binary that runs is extracted from the archive that was just verified.
	extract_binary "$work/$archive" "$work/x"

	# The entry is immutable once published, so every step is checked: errexit is
	# off in here (the function is only called from "||" and "if" contexts).
	name="$ver-$digest"
	entry="$CACHE/$name"
	# Inside the cache, so the final rename stays on one filesystem. The name is
	# unique per start: when the entry exists already, "mv" moves the stage into it.
	stage="$TMP/${TMP##*/}"
	mkdir "$stage" || die "cannot create '$stage'"
	cp "$work/$archive" "$stage/$archive" || die "cannot copy the archive into '$stage'"
	printf '%s\n' "$ARCH" >"$stage/arch" || die "cannot record the architecture"
	if [ -n "$signed" ]; then
		for f in checksums.txt checksums.txt.sigstore.json trusted_root.json; do
			cp "$work/$f" "$stage/$f" || die "cannot copy $f into '$stage'"
		done
	fi
	: >"$stage/.complete" || die "cannot mark the cache entry complete"
	acquire_lock "$name"
	if [ -d "$entry" ] && is_shaped "$entry"; then
		# Another start published it first (or an earlier start did): entries are
		# immutable and a complete entry is never replaced, so a pinned-only start
		# cannot weaken the proof of a signed one.
		rm -rf "$stage"
		if [ -n "$signed" ] && ! is_signed_entry "$entry" && trusted_node "$entry" dir; then
			# An entry made under a pin gains the proof of the signature: files are
			# added only, the archive is never touched, and the entry counts as
			# signed only when all three are there (the trusted root goes last).
			for f in checksums.txt checksums.txt.sigstore.json trusted_root.json; do
				cp "$work/$f" "$entry/.add.$f.$$" 2>/dev/null || break
				mv -f "$entry/.add.$f.$$" "$entry/$f" 2>/dev/null || break
			done
		fi
	else
		# Whatever is at the entry path is not a complete entry (debris, or an entry
		# of an earlier version of this script, which holds an unauthenticated
		# binary), and no other start can complete it while the lock is held: it is
		# moved out of the way by a rename, never deleted in place.
		if [ -e "$entry" ] || [ -L "$entry" ]; then
			mv "$entry" "$TMP/debris" || die "cannot move the unusable entry '$entry' away"
		fi
		if ! mv "$stage" "$entry" 2>/dev/null; then
			# Only a start that does not use the lock (an older script) can have
			# published the entry meanwhile; if it is complete it stays.
			is_shaped "$entry" || die "cannot publish the cache entry '$entry'"
			rm -rf "$stage"
		fi
		# "mv" moved the stage inside an entry that appeared meanwhile.
		rm -rf "${entry:?}/${stage##*/}"
	fi
	release_lock
	is_shaped "$entry" || die "the cache entry '$entry' is incomplete after publishing"
	place_binary "$work/x/minion" "$name"
}

# ----------------------------------------------------------------------- main
main() {
	parse_selector
	detect_arch
	setup_cache
	# Leftovers of a start that was killed (the entries themselves are never touched).
	find "$CACHE" -maxdepth 1 -name '.tmp.*' -mtime +1 -exec rm -rf {} + 2>/dev/null || true
	TMP=$(mktemp -d "$CACHE/.tmp.XXXXXX") || die "cannot create a temporary directory in '$CACHE'"
	trusted_node "$TMP" dir || untrusted "the temporary directory" "$TMP"

	if [ "$SEL_KIND" = exact ]; then
		dir=$(find_cached_exact "$SEL_VERSION")
		if [ -n "$dir" ]; then run_entry "$dir" "$@"; fi
		install_version "$SEL_VERSION" "$SHA" ||
			die "$SEL_VERSION is not cached and could not be downloaded (no network, or no such release)"
		exec_binary "$@"
	fi

	sel_text=${MINION_VERSION:-latest}
	need_downloader
	rc=0
	resolved=$(resolve_online) || rc=$?
	case "$rc" in
	0)
		dir=$(find_cached_exact "$resolved")
		if [ -n "$dir" ]; then run_entry "$dir" "$@"; fi
		if install_version "$resolved" ""; then exec_binary "$@"; fi
		dir=$(find_cached_channel)
		[ -n "$dir" ] ||
			die "$resolved could not be downloaded and no verified version matching '$sel_text' is cached"
		log "using the cached ${dir##*/} instead"
		run_entry "$dir" "$@"
		;;
	2)
		die "no release matches '$sel_text' (release candidates never satisfy a channel)"
		;;
	*)
		dir=$(find_cached_channel)
		[ -n "$dir" ] ||
			die "cannot resolve '$sel_text' (no network, or the release list is unavailable) and no verified version matching it is cached in '$CACHE'; connect once to fill the cache"
		log "cannot reach the release host; using the newest cached version matching '$sel_text'"
		run_entry "$dir" "$@"
		;;
	esac
}

main "$@"
