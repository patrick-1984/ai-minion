#!/bin/sh
# Install the Minion Worker APK over ADB. Pair on the device after installation.

android_main() {
	set -eu
	version=
	device=
	while [ "$#" -gt 0 ]; do
		case $1 in
		--version) [ "$#" -ge 2 ] || android_die "--version needs a release tag"; version=$2; shift ;;
		--device | -s) [ "$#" -ge 2 ] || android_die "$1 needs an ADB serial or IP:port"; device=$2; shift ;;
		*) android_die "use --version TAG and optionally --device SERIAL" ;;
		esac
		shift
	done
	command -v adb >/dev/null 2>&1 || android_die "install Android SDK Platform Tools (adb) and enable debugging on the device"
	if [ -n "$device" ]; then
		case $device in
		-*) android_die "invalid ADB serial" ;;
		*:*) adb connect "$device" ;;
		esac
		[ "$(adb -s "$device" get-state 2>/dev/null)" = device ] || android_die "authorize this computer on the Android device and rerun"
	else
		serials=$(adb devices | awk 'NR > 1 && $2 == "device" {print $1}')
		case $serials in
		'') android_die "connect a device and authorize USB or wireless debugging on it" ;;
		*'
'*) android_die "multiple authorized devices. Run adb devices and give --device SERIAL" ;;
		esac
		device=$serials
	fi
	abis=$(adb -s "$device" shell getprop ro.product.cpu.abilist | tr -d '\r\n')
	case ",$abis," in
	*,arm64-v8a,* | *,armeabi-v7a,*) ;;
	*) android_die "this device reports no supported ARM ABI. Minion requires arm64-v8a or armeabi-v7a" ;;
	esac
	sdk=$(adb -s "$device" shell getprop ro.build.version.sdk | tr -d '\r\n')
	case $sdk in
	'' | *[!0-9]*) android_die "could not read the device Android version" ;;
	esac
	[ "$sdk" -ge 29 ] || android_die "Minion requires Android 10 (API 29) or later"

	repo=patrick-1984/ai-minion
	if [ -z "$version" ]; then
		release=$(curl -fsSL "${MINION_RELEASE_API:-https://api.github.com/repos/$repo/releases}/latest")
		version=$(printf '%s\n' "$release" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
	fi
	printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || android_die "give an exact --version such as v0.17.0"
	base=${MINION_RELEASE_BASE:-https://github.com/$repo/releases/download}/$version
	apk=minion-android-${version#v}.apk
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
	curl -fsSL "$base/$apk" -o "$tmp/$apk"
	if [ -n "${MINION_CHECKSUMS_SHA256:-}" ]; then
		[ "$(android_sha "$tmp/checksums.txt")" = "$MINION_CHECKSUMS_SHA256" ] || android_die "release checksums do not match the signed manifest"
	fi
	want=$(awk -v name="$apk" '$2 == name || $2 == "*" name {print $1; exit}' "$tmp/checksums.txt")
	[ -n "$want" ] && [ "$(android_sha "$tmp/$apk")" = "$want" ] || android_die "APK checksum mismatch. Nothing was installed"
	adb -s "$device" install -r "$tmp/$apk"
	adb -s "$device" shell am start -n cc.nasz.minion/.MainActivity
	printf '%s\n' 'Complete setup in the Minion app. Paste the Hub join token on the device, or pair with a code on Android TV.'
}

android_sha() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
	else android_die "install sha256sum or shasum"
	fi
}

android_die() {
	printf 'android.sh: %s\n' "$*" >&2
	exit 1
}

{ android_main "$@"; } # minion installer complete
