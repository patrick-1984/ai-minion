# Back up and restore the Hub

Back to the [README](../../README.md).

The Hub's state is its **data directory** (`MINION_DATA_DIR`): the database (`minion.db`), the data key that protects secrets stored in it, the Hub **identity key** that Workers pin, and the generated TLS certificate and key. Workers keep their own state; there is nothing of theirs to back up on the Hub side, and a Worker that reconnects to a restored Hub with the same identity does not need to pair again.

**The backup holds the Hub's secret keys. Treat the archive like a password:** store it where only administrators can read it, encrypt it before it leaves the machine, and delete old ones. A copy of the identity key and the data key lets whoever holds it impersonate the Hub and read what the Hub stores.

## Back up

`minion hub backup` writes one `.tar.gz` archive with a consistent copy of the database (`VACUUM INTO`, safe while the Hub runs), the keys, the generated certificate and a manifest with the version and the schema version. It creates the file with mode 0600 and never overwrites an existing file.

The data directory of the [native install](../install/hub-binary.md) is mode 0700 and belongs to the account `minion-hub`, so run the command as that account, and write the archive into a directory that account can write and nobody else can read:

```sh
sudo install -d -m 0700 -o minion-hub -g minion-hub /var/backups/minion-hub
sudo -u minion-hub env MINION_DATA_DIR=/var/lib/minion-hub/data \
  minion hub backup --out /var/backups/minion-hub/minion-hub-$(date +%F).tar.gz
```

The data directory is `MINION_DATA_DIR` unless you give `--data-dir`. The archive is owned by `minion-hub` with mode 0600. To keep a copy off the machine, encrypt it first, for example `sudo -u minion-hub gpg --symmetric --output <file>.gpg <file>` (it asks for the passphrase on the terminal), and copy only the encrypted file; do not copy the plain archive to a shared place.

**Docker Hub** ([`deploy/compose/hub.yaml`](../../deploy/compose/hub.yaml)): run the same command in a one-off container of the Hub service. The container runs as UID 65532 and writes into the data directory it has mounted:

```sh
docker compose -f hub.yaml run --rm --no-deps hub hub backup --out /data/backup-$(date +%F).tar.gz
sudo install -m 0600 -o root -g root ./hub-data/backup-*.tar.gz /secure/place/
sudo rm ./hub-data/backup-*.tar.gz
```

The archive is created by the Hub's user (UID 65532) with mode 0600 in the data directory, so reading it from the host needs `sudo`, and it should not stay in the data directory. Replace `/secure/place/` by a directory only administrators can read.

Back up before every update you may want to undo: the Hub upgrades its database when it starts and there is **no database downgrade**.

## Restore

Restore into a stopped Hub. A data directory that already holds files is refused unless you give `--force`, which replaces the database, the data key, the identity key and the generated TLS material and leaves other files alone. An empty data directory needs no `--force`. An archive written by a newer `minion` (a newer schema) is refused.

Restored files are created with mode 0600 and belong to the user who ran the command, so run the restore as the user that runs the Hub, and let that user read the archive.

**Native Hub:**

```sh
sudo systemctl stop minion-hub
sudo install -m 0400 -o minion-hub -g minion-hub /secure/place/minion-hub-2026-01-31.tar.gz /var/backups/minion-hub/restore.tar.gz
sudo -u minion-hub env MINION_DATA_DIR=/var/lib/minion-hub/data \
  minion hub restore /var/backups/minion-hub/restore.tar.gz
# add --force when the data directory already holds files
sudo rm /var/backups/minion-hub/restore.tar.gz
sudo systemctl start minion-hub
```

**Docker Hub:** restore in a one-off container, which runs as UID 65532 and so creates the files with the owner the Hub needs. Give that user a private copy of the archive for the run:

```sh
docker compose -f hub.yaml stop hub
sudo install -d -m 0700 -o 65532 -g 65532 ./restore
sudo install -m 0400 -o 65532 -g 65532 /secure/place/minion-hub-2026-01-31.tar.gz ./restore/backup.tar.gz
docker compose -f hub.yaml run --rm --no-deps -v "$PWD/restore:/restore:ro" hub hub restore /restore/backup.tar.gz
# add --force before the archive name when ./hub-data already holds files
sudo rm -rf ./restore
docker compose -f hub.yaml up -d
```

After a restore, check that the identity is the one Workers pin. The identity key is private to the Hub's user, so run the command as that user: in the native install as `minion-hub`, in Docker in a container of the Hub service:

```sh
# native
sudo -u minion-hub env MINION_DATA_DIR=/var/lib/minion-hub/data minion hub identity
# Docker
docker compose -f hub.yaml run --rm --no-deps hub hub identity
```

It prints the fingerprint of the Hub identity key (52 lowercase base32 characters). Compare it with the one you recorded before, or with what `minion worker status` shows as the pinned Hub fingerprint on a Worker. The Hub keeps its identity through restarts, certificate changes and restores, so connected Workers reconnect on their own, without pairing again.

## Moving the Hub to another machine

Back up on the old machine, install the same or a newer version on the new one ([binary](../install/hub-binary.md) or [Docker](../install/hub-docker.md)), restore into its empty data directory, start it, and give it the old name (DNS, or the address of your [reverse proxy](reverse-proxy.md)). Workers reconnect to that address. Stop the old Hub first: two Hubs with one identity answer Workers from two databases.

## What a restore does not bring back

- Sessions of browsers are in the database, so users stay signed in only until their session expires; passwords and API keys are in it too, so a restore from before a revocation **brings the revoked key back**. After restoring an old backup, review users, API keys and invitation keys and revoke what should not exist.
- The Hub's record of what each Worker shares is part of the database and is also declared again by each Worker when it connects. History that the Hub holds for a session stays hidden until the Worker's current sharing rule says it may be shown, whatever the backup held.
