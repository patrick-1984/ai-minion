# AI Minion

Run and steer terminal AI agents (Claude Code, Codex and others) in tmux sessions on any machine, from one place.

- **Minion Worker**: a small agent on the machine with the sessions. It drives that machine's tmux sessions (Linux, Raspberry Pi, macOS; Windows is a preview that drives ConPTY sessions).
- **Minion Hub**: an optional multi-user server with a web UI and a REST API (one binary or one Docker image). A Worker without a Hub runs on its own (Solo): the same commands then act on the local sessions.

One binary, `minion` (also installed as `mn` by the Linux packages), is the Hub, the Worker, the command line and the MCP server.

Version 0.22.0. License: Apache-2.0.

## Start here

You need a Hub only if you want the web UI, several users or Workers on several machines.

1. **Run a Hub**: [Hub with Docker](docs/install/hub-docker.md) or [Hub as a binary](docs/install/hub-binary.md); on TrueNAS keep it current with the [host updater](docs/install/hub-truenas-updater.md). Put it behind TLS you control: [reverse proxy](docs/guides/reverse-proxy.md).
2. **Run a Worker** on each machine that has tmux sessions:
   [Linux and Raspberry Pi](docs/install/worker-linux.md),
   [macOS](docs/install/worker-macos.md),
   [Docker](docs/install/worker-docker.md),
   [Windows (preview)](docs/install/worker-windows-preview.md).
3. **Join the Worker to the Hub**: the Hub's Add worker page gives one command with the Hub address, its fingerprint and a one-time key (including a Docker variant that takes the one-time credential on standard input); a pairing code or an invitation key work too. Then choose what the Hub may see: [pairing and sharing](docs/guides/pairing.md).

### One-line install

**Linux, macOS:**

```sh
curl -fsSL https://github.com/patrick-1984/ai-minion/releases/latest/download/install.sh | sh
```

**Windows (preview, PowerShell):**

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; irm https://github.com/patrick-1984/ai-minion/releases/latest/download/install.ps1 | iex
```

**Docker (Hub):** see [Quick start: the Hub in Docker](#quick-start-the-hub-in-docker).

The scripts install the latest `minion` (`MINION_VERSION=v0.22.0` picks one) after checking it against `checksums.txt` of the release: into `/usr/local/bin` when writable or `~/.local/bin` on Linux and macOS, into `%LOCALAPPDATA%\Programs\minion` (added to your user `PATH`) on Windows. When the session host is missing, the installer installs tmux with the system package manager on Linux and macOS, or psmux with WinGet on Windows. The install guides below also show the manual install that verifies the signature.

A minimal local try-out, no Hub and no install beyond the binary and tmux:

```sh
minion worker up --share-existing all --foreground
```

In a second terminal on the same machine, this lists the sessions the Worker shares:

```sh
minion sessions_list
```

`minion --help` lists every command; `minion <command> --help` explains one.

## Quick start: the Hub in Docker

The Hub image is `ghcr.io/patrick-1984/minion` (linux/amd64, linux/arm64, linux/arm/v7). It runs as the unprivileged user UID 65532 and serves HTTPS on port 8443 with a self-signed certificate that it creates at the first start. Both ways below keep the Hub's data in `./hub-data`. The first administrator's password comes from `./admin_password.txt` with `docker run` (the file must exist before the first start) and from `MINION_ADMIN_PASSWORD` in `.env` with Compose. The details, updates and backups are in [Hub with Docker](docs/install/hub-docker.md).

### From the command line (`docker run`)

```sh
mkdir -p minion-hub/hub-data && cd minion-hub
printf '%s' 'choose-a-strong-password' > admin_password.txt

# Give the image's user (UID 65532) the data directory and the password file.
docker run --rm -v "$PWD:/w" busybox:1.37 \
  sh -c 'chown -R 65532:65532 /w/hub-data /w/admin_password.txt && chmod 0400 /w/admin_password.txt'

docker run -d --name minion-hub --restart unless-stopped \
  -p 127.0.0.1:8443:8443 \
  -v "$PWD/hub-data:/data" \
  -v "$PWD/admin_password.txt:/run/secrets/minion_admin_password:ro" \
  -e MINION_DATA_DIR=/data \
  -e MINION_ADMIN_PASSWORD_FILE=/run/secrets/minion_admin_password \
  ghcr.io/patrick-1984/minion:v0.22.0 hub serve

docker logs minion-hub
```

When the log shows the certificate fingerprint, set the Hub passphrase. The command asks for it twice on the terminal:

```sh
docker exec -it minion-hub minion hub identity set-passphrase
```

### With Docker Compose

Make a directory with `compose.yaml` and `.env`:

```sh
mkdir minion-hub && cd minion-hub
(umask 077 && touch .env)   # .env holds the admin password: readable by you only
```

`compose.yaml`, the same Hub as [`deploy/compose/hub.yaml`](deploy/compose/hub.yaml) with its settings taken from `.env`:

```yaml
services:
  # Runs once as root: gives UID 65532 the data directory, then ends.
  init:
    image: busybox:1.37
    user: "0:0"
    command: ["sh", "-c", "chown -R 65532:65532 /data"]
    volumes:
      - ./hub-data:/data

  hub:
    image: ghcr.io/patrick-1984/minion:${MINION_VERSION:-v0.22.0}
    command: ["hub", "serve"]
    restart: unless-stopped
    depends_on:
      init:
        condition: service_completed_successfully
    environment:
      MINION_DATA_DIR: /data
      # Empty means unset: the Hub then uses its default.
      MINION_ADMIN_USERNAME: ${MINION_ADMIN_USERNAME:-}
      MINION_ADMIN_PASSWORD: ${MINION_ADMIN_PASSWORD:-}
      MINION_TLS: ${MINION_TLS:-}
      MINION_PUBLIC_URL: ${MINION_PUBLIC_URL:-}
      MINION_TRUSTED_PROXIES: ${MINION_TRUSTED_PROXIES:-}
    ports:
      - "${MINION_BIND:-127.0.0.1}:${MINION_PORT:-8443}:8443"
    volumes:
      - ./hub-data:/data
```

`.env`, which Compose reads from the same directory. Every line is optional; a missing or empty value keeps the default:

```dotenv
# Image tag: an exact release, or a channel that follows new releases (0.8, 0, latest).
MINION_VERSION=v0.22.0

# Where the Hub's port is published on the host. 127.0.0.1 keeps it on this machine
# (for a reverse proxy on the same host); 0.0.0.0 publishes it on every interface.
MINION_BIND=127.0.0.1
MINION_PORT=8443

# The first administrator (default user name admin). The Hub uses the password only on the
# first start, while it has no users; you may remove it from .env afterwards. Without it the
# Hub generates a random password and logs it once. Keep the single quotes (a $ stays
# literal); the password must not contain a single quote.
MINION_ADMIN_USERNAME=admin
MINION_ADMIN_PASSWORD='choose-a-strong-password'

# Behind a reverse proxy that ends TLS (docs/guides/reverse-proxy.md), set all three and
# keep MINION_BIND=127.0.0.1: never publish a plain-HTTP Hub on a network interface.
# The trusted proxy is the exact address of the proxy, never a shared network or 0.0.0.0/0.
# MINION_TLS=off
# MINION_PUBLIC_URL=https://hub.example.com
# MINION_TRUSTED_PROXIES=172.18.0.2/32
```

Start it:

```sh
docker compose up -d
docker compose logs hub
```

When the log shows the certificate fingerprint, set the Hub passphrase. The command asks for it twice on the terminal:

```sh
docker compose exec hub minion hub identity set-passphrase
```

### Then

- Open `https://127.0.0.1:8443` on the Docker host (the browser warns about the self-signed certificate) and sign in as `admin` with the password from `admin_password.txt` (`docker run`) or `MINION_ADMIN_PASSWORD` in `.env` (Compose). The Hub reads it only at the first start, while it has no users.
- **The passphrase is required.** A new Hub serves no Workers until it is set, either with the command above or with the banner "Set the Hub passphrase" in the web UI. It has at least 14 characters and cannot be recovered: read [Hub identity](docs/guides/hub-identity.md).
- The port is published on the loopback interface only. To reach the Hub from other machines, put a reverse proxy in front ([reverse proxy](docs/guides/reverse-proxy.md); the three proxy settings are in `.env`, or `-e` options for `docker run`) or publish the port on every interface (`-p 8443:8443`, or `MINION_BIND=0.0.0.0` in `.env`).
- Add Workers: [pairing and sharing](docs/guides/pairing.md). Give each Worker the Hub fingerprint that `minion hub identity --words` prints on the Hub host (`docker exec minion-hub minion hub identity --words`), so that it pairs only with this Hub.

## Guides

Session-bound Master attachments are qualified for Claude Code 2.1.330 in a prepared Linux tmux pane with
`SO_PEERPIDFD`. An existing unprepared session needs a deliberate launch with `exec minion master claude`.
A neutral Minion MCP entry or historical tool definitions can remain in Claude after revocation. The Hub
denies later calls from the revoked attachment even if Claude presents a cached tool. Follow the
[session-bound setup guide](docs/guides/agents.md#prepare-one-claude-code-terminal).

| Guide | What it covers |
| --- | --- |
| [Desktop tray](docs/guides/tray.md) | Optional tray for Windows, Ubuntu desktop and macOS: start, stop, pause, set up the Hub, shared sessions, block new sessions, Hub key words; the menu-to-command table |
| [Pairing and sharing](docs/guides/pairing.md) | Pairing codes, invitation keys, the Hub fingerprint (`--hub-fingerprint`, `minion worker verify-hub`), choosing which sessions the Hub may see, the local share lock |
| [Reverse proxy](docs/guides/reverse-proxy.md) | TLS in front of the Hub, WebSocket and event-stream settings, trusted proxies |
| [No direct route](docs/guides/no-direct-route.md) | A Worker that cannot reach the Hub directly: SSH tunnels |
| [Sign in with OIDC](docs/guides/oidc.md) | An identity provider with role claims |
| [API keys and MCP](docs/guides/api-keys-and-mcp.md) | Scoped API keys, the CLI against a Hub, the read-only MCP server |
| [Claude Code without per-command prompts](docs/guides/claude-code-standing-allow.md) | Using Minion from Claude Code without per-command prompts: `minion claude allow`, `status`, `revoke`, the target limit of `minion mcp`, the audit and the security trade-off |
| [The pipe](docs/guides/pipe.md) | Sending a message to an agent and reading its replies |
| [AI agents](docs/guides/agents.md) | `minion llm-help`, the Claude Code and Codex plugin (`/plugin marketplace add patrick-1984/ai-minion`), the Minion master |
| [Access: who reaches what](docs/guides/access.md) | Start here for access between Workers: grants from one session to another (View, Message, Control), who may decide, Claimed and Verified sessions, the Access pages (matrix, Grants, Why?), revoking, why a call is refused |
| [Minion control](docs/guides/minion-control.md) | Master sessions (the broad form of access), which Workers accept control from which, user-managed Workers (Access > Minion control, 0.9.0) |
| [Session control](docs/guides/session-control.md) | The calls a session with access makes to list, read and type into sessions on other Workers, and the 0.8.0 operations `session_control_list` and `session_control_set` |
| [Hub identity](docs/guides/hub-identity.md) | The sealed root key and its passphrase, certificate re-issue, master-key rotation, `minion hub identity` |
| [Usage and subscription limits](docs/guides/usage.md) | Harness inventory, session and account telemetry, token buckets, native collectors and enrollment notice |
| [Log export](docs/guides/observability.md) | Sending the Hub's and the Workers' logs to syslog (RFC 5424, TLS) and OpenTelemetry (OTLP/HTTP), per-sink levels, what is masked |
| [ESP32 and ESPHome](docs/guides/esphome.md) | Install an ESP32 Worker, bridge an ESPHome device over UART, and consume its console, log and UART sessions |
| [Local integration](docs/guides/integrate-local.md) | How other programs on the machine see whether a Worker runs (user or root), its version, and nominated usage meters for a progress bar: paths, modes, schemas, the checks a reader must make, examples |
| [Control a Worker](docs/guides/worker-control.md) | `minion worker start`, `pause` and `resume`, blocking new sessions (`allow_new_sessions`), the join token from standard input (`up --join -`) |
| [Troubleshooting](docs/guides/troubleshooting.md) | Verbose logs (`-v`, `MINION_VERBOSE`), `minion worker status --json`, Administration > Diagnostics and `hub_diagnostics` |
| [Backup and restore](docs/guides/backup-restore.md) | The Hub's data, keys and identity |
| [Security and trust model](docs/security.md) | What the Hub sees, what to trust, what is not reviewed yet |

Deploy examples: [`deploy/compose/hub.yaml`](deploy/compose/hub.yaml) (Hub from the registry image), [`deploy/compose/hub-bootstrap.yaml`](deploy/compose/hub-bootstrap.yaml) (Hub from an ordinary base image, no registry pull), [`deploy/compose/worker.yaml`](deploy/compose/worker.yaml) (a Worker in a container).

## Status and limits

- The Windows Worker is a **preview**. See [its guide](docs/install/worker-windows-preview.md) for what works.
- The pairing handshake has not had an independent cryptographic review yet. See [security](docs/security.md).
- A Worker is protected against a proxy that turns malicious only when its Hub fingerprint (or invitation key) came from the Hub host; the web UI, and the CLI and MCP without a TLS key pin, are not. See [what is protected](docs/security.md#what-is-protected-against-a-proxy-that-turns-malicious). When upgrading, upgrade the Hub first.
- The Hub sees the content of the sessions a Worker shares. A reverse proxy that ends TLS for the web UI is trusted with that content.
- A message sent to an agent can make that agent run commands. Read [security](docs/security.md) before you give anyone, or any API key, the right to send.
- Replies from agents (the pipe) need tmux 3.8 or newer on the Worker. See [the pipe](docs/guides/pipe.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
