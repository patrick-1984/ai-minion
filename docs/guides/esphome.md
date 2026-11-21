# ESP32 and ESPHome

## Install the Minion firmware

Connect an ESP32, ESP32-S3, ESP32-C3 or ESP32-C6 with at least 4 MB of flash to
your computer over USB. Install Python 3 and uv, pipx or esptool 4.x. Replace the
example port with the serial port assigned to your board.

```sh
curl -fsSL https://github.com/patrick-1984/ai-minion/releases/latest/download/esp32.sh | sh -s -- --port /dev/ttyUSB0
```

The script detects the chip and flash size, checks the firmware image against
the release checksums and inspects the installed layout. On a previously used
board, supply `--replace-firmware` and confirm at the prompt to overwrite its
firmware. On a board already running Minion, supply `--update` to write only the
application and retain pairing, identity and settings.

Open the board’s USB serial console at 115200 baud. Run these commands there.

```text
wifi set YourNetwork
config set hub https://hub.example.com
reboot
enroll
status
```

Enter the Wi-Fi password and the Hub invitation key at the board’s hidden
prompts. Create the invitation from **Invitations** in the Hub. Compare the Hub
fingerprint shown by the board with the fingerprint in the Hub, then approve
the matching Worker in **Workers**. Use `status` again if Wi-Fi, time sync or
enrollment is still in progress. The [firmware guide](../../clients/esp32/README.md)
describes the local console and supported commands.

## Keep ESPHome and bridge its logs

Use a separate ESP32 running Minion. On the ESPHome board, send logger output
to a hardware UART that is routed to a pin. For example, on a board that routes
logger UART0 TX to a free header pin:

```yaml
logger:
  hardware_uart: UART0
  baud_rate: 115200
```

Check your ESPHome board’s pinout and use a UART that is free in its existing
configuration. Connect ESPHome TX to Minion RX and connect the grounds. Use
matching logic levels or an appropriate level shifter. Leave Minion TX
physically disconnected. A Hub operator who can send bytes to the UART session
then has no wire carrying those bytes back to the ESPHome board.

Choose free pins on the Minion board. This example uses GPIO 4 for its unwired TX
and GPIO 5 for RX. Pin availability and strapping restrictions vary by chip.

```text
config set gpio_allow 4,5
uart set 115200 4 5
reboot
```

## Read and use the sessions

In **Sessions** on the Hub, choose the ESP32 Worker and open **log** for its
firmware logs, **console** for commands such as `status` and **uart** for the
ESPHome log stream when bridged. MCP and API clients can locate sessions with
`sessions_list` and read them with `session_snapshot`. Use `send_text` to run
supported commands in the **console** session when you have permission.

Minion and ESPHome run on separate devices in the UART setup. An ESPHome
external component that exposes sessions from the same chip is future work.

[Back to the README](../../README.md)
