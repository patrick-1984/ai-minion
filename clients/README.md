# Thin Workers (preview)

Small Minion Workers in C for devices where the 32 MB Go binary does not fit:

| | OpenWrt router | ESP32 family |
|---|---|---|
| What it serves | shell sessions (PTY, `/bin/ash`) | a built-in command console (GPIO, ADC, PWM, I2C, Wi-Fi, status), a serial-console bridge (`uart`), the device log |
| Size | about 50 KB package (143 KB binary on MIPS); TLS from the system `libmbedtls` | 0.98-1.05 MB application in a 2 MB slot of 4 MB flash |
| Packages / images | ipk for OpenWrt 24.10, apk for 25.12: `mips_24kc`, `mipsel_24kc`, `arm_cortex-a7_neon-vfpv4`, `aarch64_cortex-a53`, `x86_64` | `esp32`, `esp32s3`, `esp32c3`, `esp32c6`: a factory image (first flash) and an app image (updates) |

They speak the existing Worker protocol 3.0, so any Minion Hub (0.8 and later) serves them without changes.
Design and review decisions: [docs/architecture/proposals/thin-workers.md](../docs/architecture/proposals/thin-workers.md).

## Connect once, works forever

1. In the Hub, create an invitation key (Workers, Add worker, Create an invitation key; one-time is fine).
2. Give it to the device (below). The device enrolls, stores its own key and the Hub's root key, and erases the
   invitation key.
3. Approve the new Worker in the Hub (it starts pending).

After that the device reconnects by itself, forever: after reboots, network changes, Hub restarts, a re-issue of
the Hub certificate and even a rotation of the Hub master key (it follows the Hub's signed chain). Nothing on the
device expires: TLS certificates are not used for trust (the device pins the Hub root key inside the protocol), so a
proxy certificate renewal changes nothing. Only a signed revoke from the Hub (removing the Worker) wipes it.

The device needs the correct time (NTP) to check the Hub certificate's validity window; OpenWrt has it by
default, the ESP32 uses SNTP and falls back to a stored time floor.

## OpenWrt

```sh
apk add ./minion-thin_*.apk            # OpenWrt 25.12 (or: opkg install ./minion-thin_*.ipk on 24.10)
uci set minion-thin.main.hub='https://minion.example.com'
uci set minion-thin.main.invite='mnk_...'
uci set minion-thin.main.enabled='1'
uci commit minion-thin
/etc/init.d/minion-thin enable && /etc/init.d/minion-thin start
logread -e minion-thin                 # "enrolled as ...", then approve it in the Hub
```

The invitation key moves from UCI into `/etc/minion-thin/` (mode 0700) at the next start and is erased after the
enrollment. `/etc/minion-thin/` is kept across `sysupgrade`. Sessions: `uci add_list minion-thin.main.session='logs=exec logread -f'`.

## ESP32

See [esp32/README.md](esp32/README.md): flashing (the factory image once; app images for updates, so the device
keeps its identity), provisioning over the USB serial console (`wifi set`, `config set hub`, `enroll`), the
commands and the pin safety rules.

## Build and test

```sh
make -C clients/linux test             # unit tests: RFC vectors and the Go repository's golden vectors
make -C clients/linux MBEDTLS=1        # Linux binary with TLS
go build -o minion ./cmd/minion        # the Hub of this repository, then:
python3 clients/tests/e2e.py --tls --hub-bin ./minion --thin-bin clients/linux/build-tls/minion-thin
```

The end-to-end test enrolls, approves, drives sessions, re-issues the Hub certificate, rotates the master key,
restarts the Hub and the device, and revokes the Worker. CI (`.github/workflows/thin-workers.yml`) also runs the
unit tests on a big-endian CPU (s390x under qemu; MIPS big-endian was checked with the OpenWrt toolchain) and builds
every package and image; a `thin-v*` tag publishes a prerelease.

## Not in the preview

Pairing codes (use an invitation key), the pipe (agent messages), session-control calls, OTA updates for ESP32.
