#!/usr/bin/env bash
# Verifies that an executable is fit to be shipped as lib<name>.so in the
# Android app and run from nativeLibraryDir:
#
#   - ELF class and machine match the ABI, ET_DYN with the PIE flag;
#   - the interpreter is /system/bin/linker64 (arm64-v8a) or /system/bin/linker
#     (armeabi-v7a);
#   - DT_NEEDED only names libraries of the Android system (ALLOWED_NEEDED in
#     android/native/versions.lock), no RPATH/RUNPATH, no text relocations;
#   - every PT_LOAD segment is aligned to at least MAX_PAGE_SIZE (16 KiB);
#   - the stack is not executable;
#   - the NDK API level in .note.android.ident equals ANDROID_API.
#
# Usage: verify-elf.sh <abi> <file>...   (NDK_BIN names the NDK llvm bin dir;
# it defaults to $ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin.)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
lock="${LOCK_FILE:-$here/../../android/native/versions.lock}"
# shellcheck disable=SC1090
. "$lock"

[ "$#" -ge 2 ] || { echo "usage: $0 <abi> <file>..." >&2; exit 2; }
abi="$1"; shift
ndk_bin="${NDK_BIN:-${ANDROID_NDK_HOME:?set ANDROID_NDK_HOME or NDK_BIN}/toolchains/llvm/prebuilt/linux-x86_64/bin}"
readelf="$ndk_bin/llvm-readelf"
[ -x "$readelf" ] || { echo "verify-elf: $readelf not found" >&2; exit 2; }

case "$abi" in
  arm64-v8a)   class="ELF64"; machine="AArch64"; interp="/system/bin/linker64" ;;
  armeabi-v7a) class="ELF32"; machine="ARM";     interp="/system/bin/linker" ;;
  *) echo "verify-elf: unknown ABI $abi" >&2; exit 2 ;;
esac

fail=0
bad() { echo "verify-elf: $file: $*" >&2; fail=1; }

for file in "$@"; do
  [ -f "$file" ] || { bad "no such file"; continue; }
  hdr="$("$readelf" -hW "$file" 2>&1)" || { bad "not an ELF file"; continue; }
  dyn="$("$readelf" -dW "$file" 2>&1 || true)"
  seg="$("$readelf" -lW "$file" 2>&1 || true)"

  got_class="$(awk '/^ *Class:/ {print $2}' <<<"$hdr")"
  [ "$got_class" = "$class" ] || bad "class $got_class, want $class"
  got_machine="$(sed -n 's/^ *Machine: *//p' <<<"$hdr")"
  [ "$got_machine" = "$machine" ] || bad "machine $got_machine, want $machine"
  got_type="$(awk '/^ *Type:/ {print $2}' <<<"$hdr")"
  [ "$got_type" = "DYN" ] || bad "type $got_type, want DYN (position independent)"
  grep -Eq '\(FLAGS_1\).*\bPIE\b' <<<"$dyn" || bad "no PIE flag in DT_FLAGS_1"
  if [ "$abi" = armeabi-v7a ]; then
    flags="$(awk '/^ *Flags:/ {print $2}' <<<"$hdr")"
    [ $(( flags & 0xff000000 )) -eq $(( 0x05000000 )) ] || bad "ARM flags $flags are not EABI version 5"
  fi

  got_interp="$(sed -n 's/.*Requesting program interpreter: \(.*\)\]/\1/p' <<<"$seg")"
  [ "$got_interp" = "$interp" ] || bad "interpreter '$got_interp', want $interp"

  while read -r lib; do
    [ -n "$lib" ] || continue
    case " $ALLOWED_NEEDED " in
      *" $lib "*) ;;
      *) bad "DT_NEEDED $lib is not an Android system library (allowed: $ALLOWED_NEEDED)" ;;
    esac
  done < <(sed -n 's/.*(NEEDED).*Shared library: \[\(.*\)\]/\1/p' <<<"$dyn")
  grep -Eq '\((RPATH|RUNPATH)\)' <<<"$dyn" && bad "has RPATH/RUNPATH"
  grep -Eq '\(TEXTREL\)|FLAGS.*TEXTREL' <<<"$dyn" && bad "has text relocations"

  loads=0
  while read -r align; do
    loads=$((loads + 1))
    [ $(( align )) -ge "$MAX_PAGE_SIZE" ] || bad "a LOAD segment is aligned to $align, want at least $MAX_PAGE_SIZE"
  done < <(awk '$1 == "LOAD" {print $NF}' <<<"$seg")
  [ "$loads" -gt 0 ] || bad "no LOAD segment"
  if awk '$1 == "GNU_STACK" {f=$(NF-1); if (f ~ /E/) exit 1}' <<<"$seg"; then :; else bad "executable stack"; fi

  note="$("$readelf" -nW "$file" 2>&1 | tr -d '\0' || true)"
  api_hex="$(awk '/NT_ANDROID_TYPE_IDENT/ {getline; print $3; exit}' <<<"$note")"
  if [ -z "$api_hex" ]; then
    bad "no .note.android.ident (not built by the NDK?)"
  elif [ $(( 16#$api_hex )) -ne "$ANDROID_API" ]; then
    bad "built for API $(( 16#$api_hex )), want $ANDROID_API"
  fi

  [ "$fail" = 0 ] && echo "verify-elf: $abi $(basename "$(dirname "$file")")/$(basename "$file"): ok ($got_machine $got_class, interp $got_interp, needs:$(sed -n 's/.*(NEEDED).*Shared library: \[\(.*\)\]/ \1/p' <<<"$dyn" | tr -d '\n'), ${loads} LOAD >= ${MAX_PAGE_SIZE}, API $ANDROID_API)"
done
exit "$fail"
