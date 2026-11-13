# Run the Hub with Docker

Back to the [README](../../README.md).

The Hub image is `ghcr.io/patrick-1984/minion`. It holds the one `minion` binary; `hub serve` runs the Hub. It is built for linux/amd64, linux/arm64 and linux/arm/v7 on a minimal base image, and it runs as the unprivileged user **UID 65532**. A Raspberry Pi can run the Hub from this image too.

The example [`deploy/compose/hub.yaml`](../../deploy/compose/hub.yaml) runs the Hub with a bind-mounted data directory, a file for the admin password and port 8443. If you cannot pull from a registry, use [`deploy/compose/hub-bootstrap.yaml`](../../deploy/compose/hub-bootstrap.yaml) instead: an ordinary base image plus `bootstrap.sh`, which downloads and verifies the release (see its header for the hosts it contacts).

## Image tags

| Tag | Meaning |
| --- | --- |
| `v0.7.1` | exactly that release |
| `0.7`, `0`, `latest` | channels that follow the newest matching release; they move when a new release is published |

Use an exact tag for a Hub you do not want to change under you. The compose file reads `MINION_VERSION` (default `v0.7.1`).

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

5. Set the Hub passphrase. **A new Hub serves no Workers until you do**: it has no identity key yet, and the passphrase seals the one it creates and issues the first certificate (the state B0 in [Hub identity](../guides/hub-identity.md)). Either sign in as the administrator and use the banner "Set the Hub passphrase" (Administration, Hub identity; the page needs HTTPS and a sign-in from the last five minutes), or run the command in the container, which asks for the passphrase twice on the terminal:

   ```sh
   docker compose -f hub.yaml exec hub minion hub identity set-passphrase
   ```

   The command needs a terminal (`docker exec -it`; `docker compose exec` allocates one unless you pass `-T`) and refuses to read the passphrase from a pipe. It talks to the running Hub over the socket `admin.sock` in the data directory (`/data`, set by `MINION_DATA_DIR` in `hub.yaml` and inherited by `exec`). The Hub serves that socket only to the user that runs it (UID 65532 in this image) and to root, so run it as the image's default user or as root, not as another user. The passphrase has at least 14 characters and cannot be recovered; read [Hub identity](../guides/hub-identity.md) first.

Then add a Worker: [pairing and sharing](../guides/pairing.md). An infrastructure invitation key file (`MINION_INFRA_INVITE_KEY_FILE`) names a key bound to the Hub identity, which does not exist before this step: leave it unset for the first start and add it after the passphrase is set ([pairing](../guides/pairing.md)).

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

With an exact tag, change `MINION_VERSION` (an environment variable or a `.env` file next to `hub.yaml`) first. A newer Hub upgrades its database when it starts. There is no database downgrade: an older Hub refuses a database that a newer one has changed, so take a [backup](../guides/backup-restore.md) before an update you may want to undo. The Hub identity (its fingerprint) does not change, but an update from 0.6 to 0.7 is not a plain pull: see the next section.

### Upgrading from 0.6 to 0.7

Protocol 3.0 needs the Hub and all Workers upgraded together, and the upgraded Hub **does not serve any Worker until you have set the passphrase**. Do these steps in this order:

1. **Take an encrypted backup** of the 0.6 Hub ([backup and restore](../guides/backup-restore.md)). There is no database downgrade; the backup is the way back.
2. **Upgrade the Hub**: set `MINION_VERSION` to `v0.7.1` (or pull a channel tag) and run `docker compose -f hub.yaml up -d`. The Hub migrates its database and keeps the 0.6 identity key as the root, in the state B1: the fingerprint, the pairings and the `mnk_` invitation keys stay valid, but no Worker can connect yet. The web UI works and shows the banner "Set the Hub passphrase".
3. **Set the passphrase**: use the banner, or `docker compose -f hub.yaml exec hub minion hub identity set-passphrase` as in step 5 above. The Hub seals the key into `root.key.age`, removes the plain `identity.key` and issues the first certificate (180 days). From now on Workers on 0.7 can connect.
4. **Upgrade the Workers** ([Linux](worker-linux.md), [Docker](worker-docker.md)). A Worker that was updated earlier reports `proto_incompatible` and retries until the Hub is upgraded; it connects by itself once the passphrase is set. `minion update` does not check protocol compatibility on a 0.6 Worker unless you give `--hub-proto-range` ([security](../security.md#updates)), so follow this order yourself.
5. **Optional, recommended: rotate the master key** once the Workers have connected (`docker compose -f hub.yaml exec hub minion hub identity rotate-master`, or the page). Your pre-0.7 backups still hold the old key in plain form. A rotation revokes all unused invitation keys and cannot undo a theft that happened earlier.

## Behind a reverse proxy

Publish the port on the loopback interface (the default in `hub.yaml`) and set `MINION_TLS=off`, `MINION_PUBLIC_URL` and `MINION_TRUSTED_PROXIES` as described in the [reverse proxy guide](../guides/reverse-proxy.md). Do not publish a plain-HTTP Hub on a network interface.

## Back up

The data directory holds the Hub's secret keys. Use `minion hub backup`, not a copy of the running directory: see [backup and restore](../guides/backup-restore.md), which also shows how to restore into a Docker data directory.
