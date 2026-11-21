#!/bin/sh
# Flash a Minion thin Worker to a connected ESP32-family board.

esp_main() {
	set -eu
	port=
	update=0
	replace=0
	version=0.1.0-preview.1
	while [ "$#" -gt 0 ]; do
		case $1 in
		--port) [ "$#" -ge 2 ] || esp_die "--port needs a serial device"; port=$2; shift ;;
		--update) update=1 ;;
		--replace-firmware) replace=1 ;;
		--firmware-version) [ "$#" -ge 2 ] || esp_die "--firmware-version needs a version"; version=${2#thin-v}; shift ;;
		*) esp_die "use --port DEVICE [--update | --replace-firmware]" ;;
		esac
		shift
	done
	[ -n "$port" ] || esp_die "give the board's serial port with --port, for example /dev/ttyUSB0 or /dev/cu.usbserial-0001"
	[ "$update$replace" != 11 ] || esp_die "choose --update or --replace-firmware"
	printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || esp_die "invalid firmware version"
	command -v python3 >/dev/null 2>&1 || esp_die "install Python 3 and uv, pipx or esptool 4.x"
	if command -v uvx >/dev/null 2>&1; then
		set -- uvx --from esptool==4.8.1 esptool.py
	elif command -v pipx >/dev/null 2>&1; then
		set -- pipx run --spec esptool==4.8.1 esptool.py
	elif python3 -c 'import esptool; assert esptool.__version__.startswith("4.")' 2>/dev/null; then
		set -- python3 -m esptool
	else
		esp_die "install uv, pipx or esptool 4.x to access the board"
	fi
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	"$@" --port "$port" flash_id >"$tmp/detect" || {
		cat "$tmp/detect" >&2
		esp_die "could not connect to the board. Close other serial programs and check the USB connection"
	}
	chip=$(sed -n 's/^Chip is \([A-Za-z0-9-]*\).*/\1/p' "$tmp/detect" | head -n 1)
	case $chip in
	ESP32-S3*) chip=esp32s3 ;;
	ESP32-C3*) chip=esp32c3 ;;
	ESP32-C6*) chip=esp32c6 ;;
	ESP32 | ESP32-D0* | ESP32D0* | ESP32-PICO* | ESP32-U4*) chip=esp32 ;;
	*) esp_die "unsupported chip. Choose ESP32, ESP32-S3, ESP32-C3 or ESP32-C6" ;;
	esac
	mb=$(sed -n 's/^Detected flash size: \([0-9][0-9]*\) MB$/\1/p' "$tmp/detect" | head -n 1)
	[ -n "$mb" ] && [ "$mb" -ge 4 ] || esp_die "Minion needs a board with at least 4 MB of flash"
	printf 'Detected %s (%s MB flash)\n' "$chip" "$mb"

	base=${MINION_THIN_RELEASE_BASE:-https://github.com/patrick-1984/ai-minion/releases/download}/thin-v$version
	prefix=minion-thin_${version}_${chip}
	factory=${prefix}_factory-0x0.bin
	curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
	esp_image "$factory"
	"$@" --chip "$chip" --port "$port" read_flash 0 0x10000 "$tmp/boot.bin"
	layout=$(python3 - "$tmp/boot.bin" "$tmp/$factory" <<'PY'
import sys
from pathlib import Path

existing, factory = (Path(name).read_bytes() for name in sys.argv[1:])
if len(existing) != 0x10000 or len(factory) <= 0x10000:
    raise SystemExit("unexpected boot or factory image length")
if existing[0x8000:0x8C00] == factory[0x8000:0x8C00]:
    print("minion")
elif existing == b"\xff" * len(existing):
    print("blank")
else:
    print("other")
PY
)
	if [ "$update" = 1 ]; then
		[ "$layout" = minion ] || esp_die "--update requires the matching Minion partition table"
		image=${prefix}_app-0x10000.bin
		esp_image "$image"
		offset=0x10000
	else
		if [ "$layout" != blank ]; then
			[ "$replace" = 1 ] || esp_die "the board has an existing flash layout. Use --update for Minion, or --replace-firmware to overwrite the installed firmware"
			( : </dev/tty ) 2>/dev/null || esp_die "replacing the firmware needs a terminal"
			printf 'Replace the firmware on %s? Type REPLACE to continue: ' "$port" >/dev/tty
			IFS= read -r answer </dev/tty
			[ "$answer" = REPLACE ] || esp_die "firmware unchanged"
		fi
		image=$factory
		offset=0x0
	fi

	"$@" --chip "$chip" --port "$port" write_flash "$offset" "$tmp/$image"
	if [ "$update" = 1 ]; then
		printf '%s\n' 'Updated the application. The Minion identity and settings were kept.'
	else
		printf 'Firmware written. Open the serial console on %s at 115200 baud.\n' "$port"
		printf '%s\n' 'Run wifi set <ssid>, config set hub <Hub URL>, reboot, then enroll. Paste the invitation key when the board prompts.' 'Use status to see the Worker ID and Hub fingerprint, then approve the Worker in the Hub.'
	fi
}

esp_image() {
	curl -fsSL "$base/$1" -o "$tmp/$1"
	want=$(awk -v name="$1" '$2 == name || $2 == "*" name {print $1; exit}' "$tmp/SHA256SUMS")
	if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$1")
	elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$tmp/$1")
	else esp_die "install sha256sum or shasum"
	fi
	[ -n "$want" ] && [ "${got%% *}" = "$want" ] || esp_die "firmware checksum mismatch. Nothing was written"
}

esp_die() {
	printf 'esp32.sh: %s\n' "$*" >&2
	exit 1
}

{ esp_main "$@"; } # minion installer complete
