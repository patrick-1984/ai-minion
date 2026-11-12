# Run the Hub with Docker

Back to the [README](../../README.md).

The Hub image is `ghcr.io/patrick-1984/minion`. It holds the one `minion` binary; `hub serve` runs the Hub. It is built for linux/amd64, linux/arm64 and linux/arm/v7 on a minimal base image, and it runs as the unprivileged user **UID 65532**. A Raspberry Pi can run the Hub from this image too.

The example [`deploy/compose/hub.yaml`](../../deploy/compose/hub.yaml) runs the Hub with a bind-mounted data directory, a file for the admin password and port 8443. If you cannot pull from a registry, use [`deploy/compose/hub-bootstrap.yaml`](../../deploy/compose/hub-bootstrap.yaml) instead: an ordinary base image plus `bootstrap.sh`, which downloads and verifies the release (see its header for the hosts it contacts).

## Image tags

| Tag | Meaning |
| --- | --- |
| `v0.6.3` | exactly that release |
| `0.6`, `0`, `latest` | channels that follow the newest matching release; they move when a new release is published |

Use an exact tag for a Hub you do not want to change under you. The compose file reads `MINION_VERSION` (default `v0.6.3`).

## Prepare and start

1. Copy `deploy/compose/hub.yaml` to an empty directory on the host.
2. Create the admin password file. It must exist before the first start, because Docker creates a directory under that name when a bind-mounted file is missing:

   ```sh
   printf '%s' 'choose-a-strong-password' > admin_password.txt
   ```

   The Hub creates the first administrator from it. The user name is `admin` unless you set `MINION_ADMIN_USERNAME`. The password is only used while the user table is empty (the first start). Without `MINION_ADMIN_PASSWORD_FILE` the Hub generates a random password, logs it once (a working credential in the Hub's log until you change it) and requires a change at the first login; prefer the private password file above ([security](../security.md#credentials)).

3. Start it:

   ```sh
   docker compose -f hub.yaml up -d
   docker compose -f hub.yaml logs hub
   ```

4. The log shows the Hub's certificate fingerprint (SHA-256). The Hub is on `https://127.0.0.1:8443` of the host; the browser warns about the self-signed certificate until you put a certificate you control in front ([reverse proxy](../guides/reverse-proxy.md)) or configure one (`MINION_TLS_CERT_FILE` and `MINION_TLS_KEY_FILE`).

Sign in as the administrator, change the password if you are asked to, and add a Worker: [pairing and sharing](../guides/pairing.md).

## Why the data directory needs preparation

`/data` holds the database, the data key, the Hub identity key and the TLS material. The image runs as UID 65532, so the host directory mounted at `/data` must be writable by that user. A directory that Docker creates itself is owned by root, and a root-owned named volume has the same problem. `hub.yaml` has an `init` service that runs once as root, gives UID 65532 the data directory and the password file, and ends; the Hub service waits for it. To do the same by hand, without an init container:

```sh
sudo mkdir -p ./hub-data && sudo chown 65532:65532 ./hub-data
sudo chown 65532:65532 ./admin_password.txt && sudo chmod 0400 ./admin_password.txt
```

Compose passes a file-based secret as a bind mount of the host file, so the host file's owner and mode are what the container sees: the Hub (UID 65532) must be able to read it, and nobody else needs to.

When the Hub cannot write its data directory it stops before it touches the disk and prints how to fix it. For a container user the message is:

```text
Error: data dir /data is not writable by uid 65532; run: chown 65532:65532 <host dir> (permission denied; in Docker or Compose run it on the host directory mounted at /data, or use an init container that does the same)
```

The Hub builds this text from the directory and the user it runs as. If the directory does not exist and cannot be created (for example `/data` with nothing mounted), the message says `data dir ... does not exist and uid 65532 cannot create it` and tells you to create the host directory, change its owner and mount it.

## Update

```sh
docker compose -f hub.yaml pull
docker compose -f hub.yaml up -d
```

With an exact tag, change `MINION_VERSION` (an environment variable or a `.env` file next to `hub.yaml`) first. A newer Hub upgrades its database when it starts. There is no database downgrade: an older Hub refuses a database that a newer one has changed, so take a [backup](../guides/backup-restore.md) before an update you may want to undo. Workers reconnect by themselves; the Hub identity does not change.

## Behind a reverse proxy

Publish the port on the loopback interface (the default in `hub.yaml`) and set `MINION_TLS=off`, `MINION_PUBLIC_URL` and `MINION_TRUSTED_PROXIES` as described in the [reverse proxy guide](../guides/reverse-proxy.md). Do not publish a plain-HTTP Hub on a network interface.

## Back up

The data directory holds the Hub's secret keys. Use `minion hub backup`, not a copy of the running directory: see [backup and restore](../guides/backup-restore.md), which also shows how to restore into a Docker data directory.
