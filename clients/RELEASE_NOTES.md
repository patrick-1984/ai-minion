Preview of the **thin Workers**: small Minion Workers in C for OpenWrt routers and ESP32 chips. They speak the
existing Worker protocol 3.0, so a Minion Hub 0.8 or later serves them without changes. This is a preview: not
part of the `v*` releases, not "latest".

**Connect once, works forever.** Enroll with a one-time invitation key, approve in the Hub, done. The device pins
the Hub root key; certificate re-issues, master-key rotations, Hub restarts and proxy certificate renewals need no
action on the device. Only a signed revoke from the Hub wipes it.

**OpenWrt** (`minion-thin_*_openwrt-<release>_<arch>.{ipk,apk}`): about 50 KB, shell sessions, procd + UCI,
state kept across sysupgrade. ipk for 24.10, apk for 25.12; mips_24kc, mipsel_24kc, arm_cortex-a7_neon-vfpv4,
aarch64_cortex-a53, x86_64. Setup: `clients/README.md`.

**ESP32** (`minion-thin_*_<chip>_factory-0x0.bin` for the first flash, `*_app-<offset>.bin` for updates that keep
the device identity): a command console for the Hub (GPIO, ADC, PWM, I2C, Wi-Fi, status), a serial-console bridge
(`uart`) and the device log as sessions; admin commands only on the local USB serial console; pins only from an
allowlist. Setup: `clients/esp32/README.md`.

Verified: unit tests with the Go implementation's golden vectors (also on big-endian MIPS), and an end-to-end test
against the Hub (enroll, approve, sessions, typing, certificate re-issue, master rotation, Hub restart, revoke) on
x86_64 and on the OpenWrt MIPS binary under qemu. ESP32 images are built, not tested on hardware in CI.

Check the files against `SHA256SUMS`. Design: `docs/architecture/proposals/thin-workers.md`.
