# Run the Hub as a binary

Back to the [README](../../README.md).

The Hub is the `minion` binary with the command `minion hub serve`. Use this guide for a machine without Docker, or when you want to run the Hub under your own service manager. For Docker see [the Docker guide](hub-docker.md).

## Get the binary

Releases are published on `https://github.com/patrick-1984/ai-minion/releases`. Each release has, per platform, an archive `minion_<version>_<os>_<arch>.tar.gz` (a `.zip` for Windows, which is a preview and is not documented for the Hub), the packages for Linux, `checksums.txt`, its signature bundle `checksums.txt.sigstore.json` and an SBOM per archive. Pick the archive for your machine: `linux` and `darwin` with `amd64` or `arm64`, and `linux` with `armv7` for a 32-bit Raspberry Pi.

Verify what you downloaded before you run it. The checksum list is signed by the release workflow of this project (keyless signature, recorded in the Sigstore transparency log):

```sh
VERSION=0.6.3
ARCHIVE=minion_${VERSION}_linux_amd64.tar.gz
BASE=https://github.com/patrick-1984/ai-minion/releases/download/v${VERSION}
curl -fsSLO "$BASE/$ARCHIVE"
curl -fsSLO "$BASE/checksums.txt"
curl -fsSLO "$BASE/checksums.txt.sigstore.json"

# 1. the archive is the one the checksum list names
sha256sum --ignore-missing -c checksums.txt

# 2. the checksum list was signed by this project's release workflow (needs cosign)
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/patrick-1984/ai-minion/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

`minion update` and `bootstrap.sh` check exactly this identity for you, so once you have one verified binary, later updates are verified by `minion update` (see below). Step 2 is optional for a first install if you trust the download channel, but then nothing proves the archive came from the release workflow.

Install it:

```sh
tar -xzf "$ARCHIVE" minion
sudo install -m 0755 minion /usr/local/bin/minion
minion version
```

The Linux `.deb` and `.rpm` packages hold the same binary (and the Worker service unit, see [the Linux Worker guide](worker-linux.md)). Installing a package on the Hub machine is fine; it does not start a Hub.

## Run it

The Hub has no command-line flags. It reads its settings from the environment:

| Variable | Meaning |
| --- | --- |
| `MINION_DATA_DIR` | Data directory: database, keys, certificate. Must be writable by the user that runs the Hub. The default is the user's data directory (`~/.local/share/minion` on Linux). |
| `MINION_LISTEN` | Listen address, default `:8443`. |
| `MINION_TLS` | `auto` (default: HTTPS with a generated self-signed certificate) or `off` (plain HTTP behind a proxy; needs `MINION_PUBLIC_URL`). |
| `MINION_TLS_CERT_FILE`, `MINION_TLS_KEY_FILE` | A certificate and key to use instead of the generated one (both or neither). `SIGHUP` reloads them. The key must be a plain file (or a link to one in a directory only the Hub user or root can write) owned by the Hub user with mode 0600 or 0400; the Hub refuses a key that group or others can access and never changes it. |
| `MINION_PUBLIC_URL` | The external URL, for example `https://hub.example.com`. |
| `MINION_TRUSTED_PROXIES` | Comma-separated addresses or CIDRs of the reverse proxies whose `X-Forwarded-*` headers are trusted. |
| `MINION_ADMIN_USERNAME`, `MINION_ADMIN_PASSWORD_FILE` | The first administrator (used only while the user table is empty). `MINION_ADMIN_PASSWORD` also works, but a file keeps the password out of the process environment. |

A first start, from a shell:

```sh
sudo useradd --system --home /var/lib/minion-hub --create-home --shell /usr/sbin/nologin minion-hub
sudo install -d -m 0700 -o minion-hub -g minion-hub /var/lib/minion-hub/data
printf '%s' 'choose-a-strong-password' | sudo -u minion-hub tee /var/lib/minion-hub/admin_password >/dev/null
sudo chmod 0400 /var/lib/minion-hub/admin_password

sudo -u minion-hub env \
  MINION_DATA_DIR=/var/lib/minion-hub/data \
  MINION_ADMIN_PASSWORD_FILE=/var/lib/minion-hub/admin_password \
  minion hub serve
```

On the first start the Hub creates its data directory content, a self-signed certificate (the log shows its SHA-256 fingerprint), its identity key and the administrator. `minion hub identity` prints the identity fingerprint: 52 lowercase base32 characters. Workers pin that identity; it does not change on restart, certificate change or restore.

To run it as a service, write a systemd unit of your own, for example `/etc/systemd/system/minion-hub.service` (this file is an example, the packages do not ship a Hub unit):

```ini
[Unit]
Description=Minion Hub
After=network-online.target
Wants=network-online.target

[Service]
User=minion-hub
Environment=MINION_DATA_DIR=/var/lib/minion-hub/data
Environment=MINION_ADMIN_PASSWORD_FILE=/var/lib/minion-hub/admin_password
ExecStart=/usr/local/bin/minion hub serve
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now minion-hub
```

Then sign in at `https://<host>:8443` and add Workers: [pairing and sharing](../guides/pairing.md). To put TLS you control in front, see the [reverse proxy guide](../guides/reverse-proxy.md).

## Update

```sh
sudo minion update --channel 0.6      # the newest 0.6.x
sudo minion update --version v0.6.3   # exactly this version
sudo systemctl restart minion-hub
```

Release 0.6.3 uses wire protocol 2.0 only: a Hub and a Worker of an earlier build cannot connect, so there are no mixed versions. Update the Hub and every Worker together. Take an encrypted backup first; the database migration is forward-only.

`minion update` verifies the release signature and the archive checksum before it replaces the binary, and refuses an older version unless you pass `--allow-downgrade`. An update contacts the release host and the Sigstore TUF repository; `--offline` installs the newest verified release from the local cache instead. The Hub upgrades its database at start. There is no database downgrade, so take a [backup](../guides/backup-restore.md) first.
