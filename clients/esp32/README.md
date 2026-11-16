# Minion thin Worker for ESP32 (preview)

An ESP-IDF v5.4 application that runs the portable C core (`../core`) and makes an ESP32-family
microcontroller a Minion Worker. The device has no shell, so it brings its own sessions:

| Session   | What it is                                                         | Input from the Hub        |
|-----------|--------------------------------------------------------------------|---------------------------|
| `console` | built-in command shell, prompt `esp> `                             | `send_text` + Enter       |
| `log`     | ESP-IDF log output (the serial port keeps printing it too)         | none (read-only)          |
| `uart`    | serial bridge to an attached device on UART1 (off until configured) | raw bytes and keys        |

Chips: ESP32, ESP32-S3, ESP32-C3, ESP32-C6 with 4 MB flash or more. The protocol is the Worker
protocol 3.0 (see `docs/architecture/proposals/thin-workers.md`): the Hub needs no change.

## Build

```sh
. $IDF_PATH/export.sh                     # ESP-IDF v5.4.x
cd clients/esp32
idf.py -B build-esp32c3 -D SDKCONFIG=build-esp32c3/sdkconfig set-target esp32c3
idf.py -B build-esp32c3 -D SDKCONFIG=build-esp32c3/sdkconfig build
# merged image for a first install (bootloader + partition table + app, from 0x0):
cd build-esp32c3 && esptool.py --chip esp32c3 merge_bin -o minion-thin-esp32c3-merged.bin @flash_args
```

Use one build directory per chip (`esp32`, `esp32s3`, `esp32c3`, `esp32c6`).

## Flash layout (`partitions.csv`, 4 MB)

| Name       | Offset     | Size    | Content                                                     |
|------------|------------|---------|-------------------------------------------------------------|
| `nvs`      | `0x9000`   | 16 KB   | Wi-Fi driver / PHY data only                                |
| `phy_init` | `0xf000`   | 4 KB    | RF calibration                                              |
| `factory`  | `0x10000`  | 2 MB    | the application (no OTA yet)                                |
| `minion`   | `0x3f0000` | 64 KB   | **the device identity** (key seed, Hub pin, ratchet, share rule) and the config (Wi-Fi, Hub URL, pins, time floor) |

The `minion` partition sits at a fixed place, the last 64 KB of the flash, outside the range of the
merged image.

## Flashing

**First install:** write the merged image at `0x0`, with `esptool.py` or a web flasher:

```sh
esptool.py --chip esp32c3 -p /dev/ttyUSB0 write_flash 0x0 minion-thin-esp32c3-merged.bin
```

**Updates: write only the app image at the factory offset `0x10000`.** That keeps the `minion`
partition, i.e. the identity, untouched:

```sh
esptool.py --chip esp32c3 -p /dev/ttyUSB0 write_flash 0x10000 build-esp32c3/minion-thin.bin
# or: idf.py -B build-esp32c3 -p /dev/ttyUSB0 app-flash
```

**Rewriting the merged image erases the identity in practice:** treat it as a new install. Web
flashers and `idf.py flash`/`erase-flash` erase the whole chip first, and a merged image padded to
the flash size (`--fill-flash-size`) overwrites the `minion` partition. (A plain
`write_flash 0x0 merged.bin` without erase stops at the end of the app and would leave it in place,
but do not rely on that.) A lost identity means a new Worker in the Hub: enroll again and remove the
old entry. Never use "erase flash" for an update.

## Provisioning (local serial console)

Connect to the chip's default console (UART0 through the USB-UART bridge, or the USB-Serial-JTAG port
on boards without a bridge; `idf.py monitor` or any terminal at 115200 baud). The banner shows the
version, the state and the next steps:

```
wifi set <ssid>                     # asks for the password without echo ('-' or empty: open network)
config set hub https://hub.example.com
reboot                              # the Hub URL and the name are read at boot
enroll                              # paste the mnk_... invitation key from the Hub; it is not shown
```

Then approve the Worker in the Hub (Workers page, or `minion hub worker approve <id>`). The device
erases the invitation key after a successful enrollment and reconnects for ever (backoff 1-60 s).
`status` shows chip, uptime, reset reason, heap, Wi-Fi, IP, time + SNTP state and the Minion state
(enrolled, connected, pending approval, Worker id, Hub fingerprint, last error).

Optional settings (`config set <key> <value>`, `-` unsets; `config get` lists them):

| Key                  | Meaning                                                                 |
|----------------------|-------------------------------------------------------------------------|
| `name`               | Worker name (default `esp32-<chip>-<last 3 MAC bytes>`, e.g. `esp32-c3-a1b2c3`) |
| `gpio_allow`         | pins the GPIO/ADC/PWM/I2C/UART commands may use, e.g. `4,5,12-14` (default: none) |
| `i2c_sda`, `i2c_scl` | pins for `i2c scan`                                                     |
| `uart_tx`, `uart_rx`, `uart_baud` | the `uart` bridge (also `uart set <baud> <tx> <rx>`; reboot to add the session) |

## Commands

Available on the local console and in the Hub `console` session:

```
help | status | time | reboot
gpio list | mode <pin> in|out|in_pullup|in_pulldown|od | read <pin> | write <pin> 0|1
gpio toggle <pin> | pulse <pin> <ms 1..10000> | watch <pin> on|off
adc read <pin>                       millivolts, calibrated where the chip has calibration data (ADC1 pins)
pwm set <pin> <freq_hz 1..100000> <duty_pct 0..100> | pwm off <pin>
i2c scan                             on the configured i2c_sda/i2c_scl pins
wifi status | wifi scan | uart status | config get [key]
```

Local console only (refused from the Hub with `ERR local console only`):

```
enroll [mnk_...]  |  wifi set <ssid> [password]  |  config set <key> <value>
uart set <baud> [tx rx] | uart off  |  factory-reset ERASE [all]
```

`gpio watch` prints debounced level changes (30 ms) into the Hub `console` session. `gpio pulse` does
not block: a timer restores the level.

## Safety

- **Physical presence for administration.** Anything that changes who the device trusts or how it is
  reached (enroll, Hub URL, Wi-Fi, pin allowlist, UART pins, factory reset) works only on the local
  serial console. A Hub user with `operate` on this Worker can read status and use allowed pins, not
  re-target the device.
- **Pin allowlist.** GPIO, ADC, PWM, I2C and the UART bridge only touch pins listed in `gpio_allow`
  (empty by default). Flash/PSRAM pins, the console UART pins and the USB D-/D+ pins are always refused,
  whatever the allowlist says (ESP32: 6-11, 16-17, 1, 3; ESP32-S3: 26-37, 19-20, 43-44;
  ESP32-C3: 12-17, 18-19, 20-21; ESP32-C6: 24-30, 12-13, 16-17), plus any pin ESP-IDF reserved at run
  time. Strapping pins (ESP32 0, 2, 5, 12, 15; S3 0, 3, 45, 46; C3 2, 8, 9; C6 4, 5, 8, 9, 15) need an
  explicit `!` in the list, e.g. `config set gpio_allow 4,5,2!`.
- **Safe boot state.** Every allowed pin starts as an input without pull at boot. No output level,
  PWM or watch is restored after a reset.
- **Limits.** `gpio pulse` at most 10 s; PWM 1 Hz .. 100 kHz (very low frequencies may not be reachable
  on 14-bit LEDC chips); ADC2 is refused (unusable while Wi-Fi runs).
- **Secrets.** The Wi-Fi password and the invitation key are never echoed to a session or written to
  the log; the local console prompts for them without echo, masks them as `***` in its output and
  drops the command history line that carried them. `config get` never prints them. Typing them into
  the Hub `console` session echoes `*` only (and the command is refused there anyway).
- **The `uart` bridge** to a router's serial console is a root shell of that router for every Hub user
  with `operate` on this Worker. It is off until its pins are set locally.
- **Identity at rest.** The key seed sits in NVS in plain form. Anyone with the device in hand can read
  it; enable flash encryption + NVS encryption for devices in untrusted places.
- **Entropy.** The identity seed is generated only after Wi-Fi has started (RF on: the hardware RNG is
  a true RNG).
- **Clock.** The Hub certificate window needs wall time. The device uses SNTP (the server offered by
  DHCP and `pool.ntp.org`) and keeps a time floor in NVS (written on SNTP sync, at most once per hour).
  When SNTP has not answered 30 s after the IP came up, the clock is set to that floor (a warning is
  logged), so a device on a network without NTP can still connect after it once had time.
- **TLS is not verified on purpose**: trust comes from the Hub root key pinned at enrollment, checked
  inside the Worker protocol. SNI is sent so reverse proxies can route. TLS 1.3 is enabled (the Hub's
  own listener accepts nothing older); TLS 1.2 stays available for proxies.

## Resources

App image about 0.98-1.05 MB (2 MB partition). Session screens: 80x24 each with 2-3 KB of scrollback.
The worker task has a 10 KB stack (measured peak 6.7 KB). The log line `connected: 8-bit heap free ...`
reports the free heap and the worker stack reserve after each connect; `status` shows both at any time.
RAM is tightest on the ESP32-C3: watch `min free` there.

Limits on this device: an operation request up to 2 KB (a longer `send_text` is answered
`message_too_large`), results up to 8 KB, 4 concurrent streams, 16 remembered write results, and a Hub
hello with up to about 40 root transition links (`MT_HS_MAX_TOKENS=512`).

## Emulator test (QEMU, ESP32)

`sdkconfig.qemu` builds a variant that uses QEMU's OpenCores Ethernet instead of Wi-Fi (never flash it
to a device):

```sh
idf.py -B build-qemu -D SDKCONFIG=build-qemu/sdkconfig -D SDKCONFIG_DEFAULTS="sdkconfig.defaults;sdkconfig.qemu" set-target esp32 build
idf.py -B build-qemu qemu monitor          # the Hub on the host is https://10.0.2.2:<port>
```

In the emulator `config set hub` starts the Worker at once (no reboot needed: QEMU's Ethernet does not
survive a software reset). Wi-Fi commands report errors there.
