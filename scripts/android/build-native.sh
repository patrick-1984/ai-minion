#!/usr/bin/env bash
# Builds the native parts of the Minion Android app, per ABI:
#
#   libminion.so  the Worker (cmd/minion) with CGO_ENABLED=1, the NDK clang and
#                 the cgo resolver (Go prefers it on Android; pure Go has no
#                 working DNS there, and android/arm needs external linking)
#   libtmux.so    tmux with libevent and ncurses linked in statically
#   terminfo/     the terminfo database (arch independent, built once)
#
# Everything comes from android/native/versions.lock: pinned NDK, native API
# floor and source hashes. Sources are downloaded and verified against their
# SHA-256; the patches in android/native/patches/ are applied with zero fuzz.
# The result is checked by scripts/android/verify-elf.sh (ABI, PIE, interpreter,
# DT_NEEDED closure, 16 KiB LOAD alignment).
#
# Usage: scripts/android/build-native.sh [--abi ABI]... [--out DIR] [--work DIR]
#                                        [--version V] [--fetch-ndk]
#   --abi        arm64-v8a or armeabi-v7a (repeatable; default: every ABI)
#   --out        new or empty output directory (default: dist/android-native)
#   --work       scratch and download directory (default: .android-native-work)
#   --version    version string injected into the Worker (default: dev)
#   --fetch-ndk  cache the pinned NDK archive in WORK/dl and use it (otherwise
#                ANDROID_NDK_HOME must be the pinned revision)
# Needs: bash, curl, tar, unzip, make, patch, gcc (host tools of ncurses), go.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
lock="$root/android/native/versions.lock"
# shellcheck disable=SC1090
. "$lock"

abis=()
out="$root/dist/android-native"
work="$root/.android-native-work"
version="dev"
fetch_ndk=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --abi) abis+=("$2"); shift 2 ;;
    --out) out="$2"; shift 2 ;;
    --work) work="$2"; shift 2 ;;
    --version) version="$2"; shift 2 ;;
    --fetch-ndk) fetch_ndk=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "build-native: unknown argument $1" >&2; exit 2 ;;
  esac
done
# shellcheck disable=SC2153
[ "${#abis[@]}" -gt 0 ] || read -r -a abis <<<"$ABIS"
log() { echo "build-native: $*" >&2; }
die() { echo "build-native: $*" >&2; exit 1; }

mkdir -p "$(dirname "$out")" "$work/dl"
out="$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
cache="$(cd "$work" && pwd)/dl"
[ ! -L "$out" ] || die "output must not be a symbolic link"
if [ -e "$out" ]; then
  [ -d "$out" ] && [ -z "$(ls -A "$out")" ] || die "output must be new or empty: $out"
fi
stage="$(mktemp -d "$(dirname "$out")/.android-native.XXXXXXXX")"
trap 'rm -rf "${stage:?}"' EXIT
work="$(mktemp -d "$(dirname "$cache")/build.XXXXXXXX")"
cleanup() {
  local status=$?
  rm -rf "${stage:?}"
  if [ "$status" = 0 ]; then rm -rf "${work:?}"; fi
}
trap cleanup EXIT
publish="$out"
out="$stage"
jobs="$(nproc 2>/dev/null || echo 2)"

# Verify the archive on every use. The lock is shared by concurrent builds
# using the same download cache.
fetch() (
  local url="$1" sha="$2" file="$3"
  flock 9
  if [ ! -f "$file" ]; then
    log "download $url"
    curl -fsSL --connect-timeout 20 --max-time 600 --retry 3 --retry-max-time 900 -o "$file.part" "$url"
    mv "$file.part" "$file"
  fi
  echo "$sha  $file" | sha256sum -c --quiet - || { rm -f "${file:?}"; die "SHA-256 mismatch for $url"; }
) 9>"$3.lock"

# --- NDK ----------------------------------------------------------------------
# --fetch-ndk always uses the pinned download (CI images preset ANDROID_NDK_HOME
# with another NDK revision).
if [ "$fetch_ndk" = 1 ] || [ -z "${ANDROID_NDK_HOME:-}" ]; then
  [ "$fetch_ndk" = 1 ] || die "set ANDROID_NDK_HOME to NDK $NDK_VERSION, or pass --fetch-ndk"
  ndk_dir="$work/ndk"
  ndk_zip="$cache/android-ndk-$NDK_VERSION-linux.zip"
  fetch "$NDK_URL" "$NDK_SHA256" "$ndk_zip"
  mkdir -p "$ndk_dir"
  unzip -q "$ndk_zip" -d "$ndk_dir"
  ANDROID_NDK_HOME="$ndk_dir/android-ndk-$NDK_VERSION"
fi
rev="$(sed -n 's/^Pkg.Revision *= *//p' "$ANDROID_NDK_HOME/source.properties" 2>/dev/null || true)"
[ "$rev" = "$NDK_REVISION" ] || die "the NDK at $ANDROID_NDK_HOME is revision '$rev', versions.lock pins $NDK_REVISION ($NDK_VERSION)"
export ANDROID_NDK_HOME
tc="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64"
export NDK_BIN="$tc/bin"

# --- Sources, patches ---------------------------------------------------------
src="$work/src"
mkdir -p "$src"
unpack() { # NAME VERSION URL SHA
  local name="$1" ver="$2" url="$3" sha="$4"
  local archive="$cache/$name-$ver.tar.gz"
  fetch "$url" "$sha" "$archive"
  mkdir -p "$src/$name-$ver"
  tar xzf "$archive" -C "$src/$name-$ver" --strip-components=1
}
unpack tmux "$TMUX_VERSION" "$TMUX_URL" "$TMUX_SHA256"
unpack libevent "$LIBEVENT_VERSION" "$LIBEVENT_URL" "$LIBEVENT_SHA256"
unpack ncurses "$NCURSES_VERSION" "$NCURSES_URL" "$NCURSES_SHA256"

apply_patches() {
  local dir="$1" pdir="$root/android/native/patches" listed=() f sha p
  while read -r f sha _; do
    case "$f" in ''|'#'*) continue ;; esac
    [ -f "$pdir/$f" ] || die "patches/series lists $f, which does not exist"
    echo "$sha  $pdir/$f" | sha256sum -c --quiet - || die "patches/$f does not match its hash in series"
    listed+=("$f")
    log "apply $f (zero fuzz)"
    (cd "$dir" && patch -p1 --fuzz=0 --no-backup-if-mismatch < "$pdir/$f") || die "patches/$f does not apply exactly to tmux $TMUX_VERSION"
  done < "$pdir/series"
  for p in "$pdir"/*.patch "$pdir"/*.diff; do
    [ -e "$p" ] || continue
    case " ${listed[*]:-} " in *" $(basename "$p") "*) ;; *) die "patches/$(basename "$p") is not listed in series" ;; esac
  done
}
apply_patches "$src/tmux-$TMUX_VERSION"

# --- Host tic (terminfo is compiled with the pinned ncurses, not the host's) --
host="$work/host"
log "build the host tic of ncurses $NCURSES_VERSION"
mkdir -p "$work/build/ncurses-host" "$host"
(
  cd "$work/build/ncurses-host"
  "$src/ncurses-$NCURSES_VERSION/configure" --prefix="$host" --without-shared --with-normal \
    --without-debug --without-cxx --without-cxx-binding --without-ada --without-manpages --without-tests \
    --disable-stripping --enable-widec --without-gpm > configure.log 2>&1
  make -j"$jobs" -C include > make.log 2>&1
  make -j"$jobs" -C ncurses >> make.log 2>&1
  make -j"$jobs" -C progs tic >> make.log 2>&1
  install -D progs/tic "$host/bin/tic"
) || die "the host tic did not build (see $work/build/ncurses-host/*.log)"

# --- terminfo -----------------------------------------------------------------
log "compile the terminfo entries: $TERMINFO_ENTRIES"
mkdir -p "$out/terminfo"
"$host/bin/tic" -x -e "$TERMINFO_ENTRIES" -o "$out/terminfo" "$src/ncurses-$NCURSES_VERSION/misc/terminfo.src"
IFS=, read -r -a want <<<"$TERMINFO_ENTRIES"
for t in "${want[@]}"; do
  [ -f "$out/terminfo/${t:0:1}/$t" ] || die "terminfo entry $t was not compiled"
done


# --- One ABI ------------------------------------------------------------------
build_abi() {
  local abi="$1" clang_prefix host_triple goarch goarm="" baseline
  case "$abi" in
    arm64-v8a)   clang_prefix=aarch64-linux-android;    host_triple=aarch64-linux-android; goarch=arm64; baseline=-march=armv8-a ;;
    armeabi-v7a) clang_prefix=armv7a-linux-androideabi; host_triple=arm-linux-androideabi; goarch=arm; goarm=7; baseline=-march=armv7-a ;;
    *) die "unknown ABI $abi" ;;
  esac
  local cc="$NDK_BIN/${clang_prefix}${ANDROID_API}-clang"
  [ -x "$cc" ] || die "$cc not found"
  local prefix="$work/prefix/$abi" b="$work/build/$abi"
  mkdir -p "$prefix" "$b"
  local cflags="$baseline -O2 -fPIC -ffile-prefix-map=$work=/build -ffile-prefix-map=$root=/src"
  local ldpage="-Wl,-z,max-page-size=$MAX_PAGE_SIZE -Wl,-z,common-page-size=$MAX_PAGE_SIZE"
  local env_cc=(env "CC=$cc" "AR=$NDK_BIN/llvm-ar" "RANLIB=$NDK_BIN/llvm-ranlib" "STRIP=$NDK_BIN/llvm-strip"
                "CPPFLAGS=" "CXXFLAGS=$cflags" "PKG_CONFIG_LIBDIR=$prefix/lib/pkgconfig" "PKG_CONFIG_PATH=" "PATH=$NDK_BIN:$PATH")

  log "[$abi] libevent $LIBEVENT_VERSION"
  mkdir -p "$b/libevent"
  (cd "$b/libevent" && "${env_cc[@]}" "$src/libevent-$LIBEVENT_VERSION/configure" --host="$host_triple" --prefix="$prefix" \
      --disable-shared --enable-static --disable-openssl --disable-samples --disable-libevent-regress --disable-debug-mode \
      CFLAGS="$cflags" LDFLAGS="$ldpage" > configure.log 2>&1 \
    && "${env_cc[@]}" make -j"$jobs" > make.log 2>&1 && "${env_cc[@]}" make install > install.log 2>&1) \
    || die "[$abi] libevent failed (see $b/libevent/*.log)"

  log "[$abi] ncurses $NCURSES_VERSION"
  mkdir -p "$b/ncurses"
  (cd "$b/ncurses" && "${env_cc[@]}" "$src/ncurses-$NCURSES_VERSION/configure" --host="$host_triple" --build="$(gcc -dumpmachine)" \
      --prefix="$prefix" --with-build-cc=gcc --without-shared --with-normal --without-debug --without-cxx --without-cxx-binding \
      --without-ada --without-manpages --without-tests --without-progs --disable-stripping --enable-widec --enable-pc-files \
      --with-pkg-config-libdir="$prefix/lib/pkgconfig" --with-termlib --disable-home-terminfo --without-gpm --without-pthread \
      --enable-overwrite --with-default-terminfo-dir=/system/etc/terminfo --with-terminfo-dirs=/system/etc/terminfo \
      CFLAGS="$cflags" LDFLAGS="$ldpage" > configure.log 2>&1 \
    && "${env_cc[@]}" make -j"$jobs" > make.log 2>&1 && "${env_cc[@]}" make install.libs install.includes > install.log 2>&1) \
    || die "[$abi] ncurses failed (see $b/ncurses/*.log)"

  log "[$abi] tmux $TMUX_VERSION"
  mkdir -p "$b/tmux"
  (cd "$b/tmux" && "${env_cc[@]}" "$src/tmux-$TMUX_VERSION/configure" --host="$host_triple" --build="$(gcc -dumpmachine)" \
      --prefix="$prefix" --with-TERM="$TMUX_DEFAULT_TERM" \
      CFLAGS="$cflags -fPIE" LDFLAGS="-pie $ldpage -Wl,-z,relro,-z,now" > configure.log 2>&1 \
    && "${env_cc[@]}" make -j"$jobs" > make.log 2>&1) || die "[$abi] tmux failed (see $b/tmux/*.log)"
  mkdir -p "$out/$abi"
  "$NDK_BIN/llvm-strip" --strip-unneeded -o "$out/$abi/libtmux.so" "$b/tmux/tmux"
  chmod 755 "$out/$abi/libtmux.so"

  log "[$abi] Worker (cgo, $cc)"
  local commit date
  commit="$(git -C "$root" rev-parse --short=12 HEAD 2>/dev/null || echo dev)"
  date="${SOURCE_DATE:-$(git -C "$root" show -s --format=%cI HEAD 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)}"
  (cd "$root" && env GOENV=off GOFLAGS= GOEXPERIMENT= GOARM64=v8.0 CGO_ENABLED=1 GOOS=android GOARCH="$goarch" GOARM="$goarm" \
      CC="$cc" CXX="${cc}++" CGO_CFLAGS="$cflags" CGO_CPPFLAGS= CGO_CXXFLAGS="$cflags" CGO_FFLAGS= CGO_LDFLAGS="$ldpage" \
      go build -tags netcgo -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/patrick-1984/ai-minion/internal/version.Version=$version -X github.com/patrick-1984/ai-minion/internal/version.Commit=$commit -X github.com/patrick-1984/ai-minion/internal/version.Date=$date" \
      -o "$out/$abi/libminion.so" ./cmd/minion) || die "[$abi] the Worker did not build"
  chmod 755 "$out/$abi/libminion.so"

  "$cc" "$baseline" -O2 -fPIE -pie "-Wl,-z,max-page-size=$MAX_PAGE_SIZE" \
    "-Wl,-z,common-page-size=$MAX_PAGE_SIZE" -Wl,-z,relro,-z,now -s \
    -o "$out/$abi/libminion-launch.so" "$root/android/native/launch.c"
  chmod 755 "$out/$abi/libminion-launch.so"

  local verifier="$root/scripts/android/verify-elf.sh" other=arm64-v8a rejected
  "$verifier" "$abi" "$out/$abi/"lib*.so
  [ "$abi" != arm64-v8a ] || other=armeabi-v7a
  rejected=0; "$verifier" "$other" "$out/$abi/libtmux.so" >/dev/null 2>&1 || rejected=$?
  [ "$rejected" = 1 ] || die "[$abi] wrong-ABI verification did not reject the sample"
  rejected=0; "$verifier" "$abi" /bin/true >/dev/null 2>&1 || rejected=$?
  [ "$rejected" = 1 ] || die "[$abi] host ELF verification did not reject the sample"
}

for abi in "${abis[@]}"; do build_abi "$abi"; done

# --- Notices and manifest -----------------------------------------------------
mkdir "$out/licenses"
cp "$src/tmux-$TMUX_VERSION/COPYING" "$out/licenses/tmux-COPYING.txt"
cp "$src/libevent-$LIBEVENT_VERSION/LICENSE" "$out/licenses/libevent-LICENSE.txt"
cp "$src/ncurses-$NCURSES_VERSION/COPYING" "$out/licenses/ncurses-COPYING.txt"
(
  cd "$out"
  {
    echo "ndk $NDK_VERSION ($NDK_REVISION) api $ANDROID_API"
    echo "tmux $TMUX_VERSION libevent $LIBEVENT_VERSION ncurses $NCURSES_VERSION"
    for abi in "${abis[@]}"; do
      go version "$abi/libminion.so"
    done
  } > native-manifest.txt
  find "${abis[@]}" terminfo licenses native-manifest.txt -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
  sha256sum -c --quiet SHA256SUMS
)
mv -T "$out" "$publish"
log "done: $publish"
