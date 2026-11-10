# AI Minion

Run and steer terminal AI agents (Claude Code, Codex and others) in tmux sessions on any machine, from one place.

- **Minion Worker**: a small agent on the machine with the sessions. It drives that machine's tmux sessions (Linux, Raspberry Pi, macOS; Windows is a preview that drives ConPTY sessions).
- **Minion Hub**: an optional multi-user server with a web UI and a REST API (one binary or one Docker image). A Worker without a Hub runs on its own (Solo): the same commands then act on the local sessions.

One binary, `minion` (also installed as `mn` by the Linux packages), is the Hub, the Worker, the command line and the MCP server.

Version 0.5.0. License: Apache-2.0.

## Start here

You need a Hub only if you want the web UI, several users or Workers on several machines.

1. **Run a Hub**: [Hub with Docker](docs/install/hub-docker.md) or [Hub as a binary](docs/install/hub-binary.md). Put it behind TLS you control: [reverse proxy](docs/guides/reverse-proxy.md).
2. **Run a Worker** on each machine that has tmux sessions:
   [Linux and Raspberry Pi](docs/install/worker-linux.md),
   [macOS](docs/install/worker-macos.md),
   [Docker](docs/install/worker-docker.md),
   [Windows (preview)](docs/install/worker-windows-preview.md).
3. **Join the Worker to the Hub** with a pairing code or an invitation key, and choose what the Hub may see: [pairing and sharing](docs/guides/pairing.md).

A minimal local try-out, no Hub and no install beyond the binary and tmux:

```sh
minion worker up --share-existing all --foreground
```

In a second terminal on the same machine, `minion sessions_list` lists the sessions the Worker shares. `minion --help` lists every command; `minion <command> --help` explains one.

## Guides

| Guide | What it covers |
| --- | --- |
| [Pairing and sharing](docs/guides/pairing.md) | Pairing codes, invitation keys, choosing which sessions the Hub may see, the local share lock |
| [Reverse proxy](docs/guides/reverse-proxy.md) | TLS in front of the Hub, WebSocket and event-stream settings, trusted proxies |
| [No direct route](docs/guides/no-direct-route.md) | A Worker that cannot reach the Hub directly: SSH tunnels |
| [Sign in with OIDC](docs/guides/oidc.md) | An identity provider with role claims |
| [API keys and MCP](docs/guides/api-keys-and-mcp.md) | Scoped API keys, the CLI against a Hub, the read-only MCP server |
| [The pipe](docs/guides/pipe.md) | Sending a message to an agent and reading its replies |
| [Backup and restore](docs/guides/backup-restore.md) | The Hub's data, keys and identity |
| [Security and trust model](docs/security.md) | What the Hub sees, what to trust, what is not reviewed yet |

Deploy examples: [`deploy/compose/hub.yaml`](deploy/compose/hub.yaml) (Hub from the registry image), [`deploy/compose/hub-bootstrap.yaml`](deploy/compose/hub-bootstrap.yaml) (Hub from an ordinary base image, no registry pull), [`deploy/compose/worker.yaml`](deploy/compose/worker.yaml) (a Worker in a container).

## Status and limits

- The Windows Worker is a **preview**. See [its guide](docs/install/worker-windows-preview.md) for what works.
- The pairing handshake has not had an independent cryptographic review yet. See [security](docs/security.md).
- The Hub sees the content of the sessions a Worker shares. A reverse proxy that ends TLS for the web UI is trusted with that content.
- A message sent to an agent can make that agent run commands. Read [security](docs/security.md) before you give anyone, or any API key, the right to send.
- Replies from agents (the pipe) need tmux 3.8 or newer on the Worker. See [the pipe](docs/guides/pipe.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
