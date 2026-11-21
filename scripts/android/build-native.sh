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
#   --out        output directory (default: dist/android-native)
#   --work       scratch and download directory (default: .android-native-work)
#   --version    version string injected into the Worker (default: dev)
#   --fetch-ndk  download the pinned NDK into WORK/ndk and use it (otherwise
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
[ "${#abis[@]}" -gt 0 ] || read -r -a abis <<<"$ABIS"
mkdir -p "$out" "$work"
out="$(cd "$out" && pwd)"
work="$(cd "$work" && pwd)"
dl="$work/dl"
mkdir -p "$dl"
jobs="$(nproc 2>/dev/null || echo 2)"

log() { echo "build-native: $*" >&2; }
die() { echo "build-native: $*" >&2; exit 1; }

# fetch URL SHA256 FILE: download once, always verify.
fetch() {
  local url="$1" sha="$2" file="$3"
  if [ ! -f "$file" ]; then
    log "download $url"
    curl -fsSL --retry 3 --retry-delay 5 -o "$file.part" "$url"
    mv "$file.part" "$file"
  fi
  echo "$sha  $file" | sha256sum -c --quiet - || { rm -f "${file:?}"; die "SHA-256 mismatch for $url"; }
}

# --- NDK ----------------------------------------------------------------------
# --fetch-ndk always uses the pinned download (CI images preset ANDROID_NDK_HOME
# with another NDK revision).
if [ "$fetch_ndk" = 1 ] || [ -z "${ANDROID_NDK_HOME:-}" ]; then
  [ "$fetch_ndk" = 1 ] || die "set ANDROID_NDK_HOME to NDK $NDK_VERSION, or pass --fetch-ndk"
  ndk_dir="$work/ndk"
  if [ ! -f "$ndk_dir/android-ndk-$NDK_VERSION/source.properties" ]; then
    fetch "$NDK_URL" "$NDK_SHA256" "$dl/android-ndk-$NDK_VERSION-linux.zip"
    rm -rf "${ndk_dir:?}"
    mkdir -p "$ndk_dir"
    unzip -q "$dl/android-ndk-$NDK_VERSION-linux.zip" -d "$ndk_dir"
  fi
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
  fetch "$url" "$sha" "$dl/$name-$ver.tar.gz"
  if [ ! -f "$src/$name-$ver/.unpacked" ]; then
    rm -rf "${src:?}/${name:?}-${ver:?}"
    mkdir -p "$src/$name-$ver"
    tar xzf "$dl/$name-$ver.tar.gz" -C "$src/$name-$ver" --strip-components=1
    touch "$src/$name-$ver/.unpacked"
  fi
}
unpack tmux "$TMUX_VERSION" "$TMUX_URL" "$TMUX_SHA256"
unpack libevent "$LIBEVENT_VERSION" "$LIBEVENT_URL" "$LIBEVENT_SHA256"
unpack ncurses "$NCURSES_VERSION" "$NCURSES_URL" "$NCURSES_SHA256"

apply_patches() {
  local dir="$1" pdir="$root/android/native/patches" listed=() f sha p
  if [ -f "$dir/.patched" ]; then return; fi
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
  touch "$dir/.patched"
}
apply_patches "$src/tmux-$TMUX_VERSION"

# --- Host tic (terminfo is compiled with the pinned ncurses, not the host's) --
host="$work/host"
if [ ! -x "$host/bin/tic" ]; then
  log "build the host tic of ncurses $NCURSES_VERSION"
  rm -rf "${work:?}/build/ncurses-host"
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
fi

# --- terminfo -----------------------------------------------------------------
if [ ! -f "$out/terminfo/.done" ]; then
  log "compile the terminfo entries: $TERMINFO_ENTRIES"
  rm -rf "${out:?}/terminfo"
  mkdir -p "$out/terminfo"
  "$host/bin/tic" -x -e "$TERMINFO_ENTRIES" -o "$out/terminfo" "$src/ncurses-$NCURSES_VERSION/misc/terminfo.src"
  IFS=, read -r -a want <<<"$TERMINFO_ENTRIES"
  for t in "${want[@]}"; do
    [ -f "$out/terminfo/${t:0:1}/$t" ] || die "terminfo entry $t was not compiled"
  done
  touch "$out/terminfo/.done"
fi

# --- One ABI ------------------------------------------------------------------
build_abi() {
  local abi="$1" clang_prefix host_triple goarch goarm=""
  case "$abi" in
    arm64-v8a)   clang_prefix=aarch64-linux-android;    host_triple=aarch64-linux-android; goarch=arm64 ;;
    armeabi-v7a) clang_prefix=armv7a-linux-androideabi; host_triple=arm-linux-androideabi; goarch=arm; goarm=7 ;;
    *) die "unknown ABI $abi" ;;
  esac
  local cc="$NDK_BIN/${clang_prefix}${ANDROID_API}-clang"
  [ -x "$cc" ] || die "$cc not found"
  local prefix="$work/prefix/$abi" b="$work/build/$abi"
  rm -rf "${prefix:?}" "${b:?}"
  mkdir -p "$prefix" "$b"
  local cflags="-O2 -fPIC -ffile-prefix-map=$work=/build -ffile-prefix-map=$root=/src"
  local ldpage="-Wl,-z,max-page-size=$MAX_PAGE_SIZE -Wl,-z,common-page-size=$MAX_PAGE_SIZE"
  local env_cc=(env "CC=$cc" "AR=$NDK_BIN/llvm-ar" "RANLIB=$NDK_BIN/llvm-ranlib" "STRIP=$NDK_BIN/llvm-strip"
                "PKG_CONFIG_LIBDIR=$prefix/lib/pkgconfig" "PKG_CONFIG_PATH=" "PATH=$NDK_BIN:$PATH")

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
  date="${SOURCE_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
  (cd "$root" && CGO_ENABLED=1 GOOS=android GOARCH="$goarch" GOARM="$goarm" CC="$cc" CGO_LDFLAGS="$ldpage" \
      go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/patrick-1984/ai-minion/internal/version.Version=$version -X github.com/patrick-1984/ai-minion/internal/version.Commit=$commit -X github.com/patrick-1984/ai-minion/internal/version.Date=$date" \
      -o "$out/$abi/libminion.so" ./cmd/minion) || die "[$abi] the Worker did not build"
  chmod 755 "$out/$abi/libminion.so"

  "$root/scripts/android/verify-elf.sh" "$abi" "$out/$abi/libminion.so" "$out/$abi/libtmux.so"
}

for abi in "${abis[@]}"; do build_abi "$abi"; done

# --- Manifest -----------------------------------------------------------------
(
  cd "$out"
  {
    echo "ndk $NDK_VERSION ($NDK_REVISION) api $ANDROID_API"
    echo "tmux $TMUX_VERSION libevent $LIBEVENT_VERSION ncurses $NCURSES_VERSION"
    echo "go $(go env GOVERSION)"
  } > native-manifest.txt
  find . -type f \( -name 'lib*.so' -o -path './terminfo/*' \) ! -name .done -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
)
log "done: $out"
