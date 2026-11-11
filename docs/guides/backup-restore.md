# Back up and restore the Hub

Back to the [README](../../README.md).

The Hub's state is its **data directory** (`MINION_DATA_DIR`): the database (`minion.db`), the data key that protects secrets stored in it, the Hub **identity key** that Workers pin, and the generated TLS certificate and key. Workers keep their own state; there is nothing of theirs to back up on the Hub side, and a Worker that reconnects to a restored Hub with the same identity does not need to pair again.

**The backup holds the Hub's secret keys.** A copy of the identity key and the data key lets whoever holds it impersonate the Hub and read what the Hub stores. For that reason `minion hub backup` **encrypts the archive by default** (with [age](https://age-encryption.org), passphrase mode): you must pass `--passphrase-file FILE`, or `--unencrypted` to opt out on purpose. Keep the passphrase apart from the archive, and delete old archives. **Without the passphrase an encrypted backup cannot be restored; there is no recovery.**

## Back up

`minion hub backup` writes one archive with a consistent copy of the database (`VACUUM INTO`, safe while the Hub runs), the keys (data key, Hub identity key, TLS key), the generated certificate and a manifest with the version and the schema version. The whole archive is a tar.gz that is then encrypted with age. It creates the file with mode 0600 and never overwrites an existing file.

The passphrase file is a private file: a regular file of the user who runs the command, with no access for group or others (mode 0600; on Windows an owner-and-SYSTEM-only ACL), and not a symbolic link. A trailing newline is ignored and an empty file is refused. Create one without putting the passphrase on a command line:

```sh
( umask 077; head -c 32 /dev/urandom | base64 > ~/minion-backup.pass )
```

Store a copy of the passphrase in your password manager. Options:

- `--passphrase-file FILE`: encrypted archive (the normal choice).
- `--unencrypted`: a plain `.tar.gz`, as `minion` 0.5 wrote. The command prints a warning, and the file gives anyone who reads it the keys of the Hub.
- Neither flag: the command refuses and names both choices. There is no environment variable that switches encryption off.

The data directory of the [native install](../install/hub-binary.md) is mode 0700 and belongs to the account `minion-hub`, so run the command as that account, and write the archive into a directory that account can write and nobody else can read:

```sh
sudo install -d -m 0700 -o minion-hub -g minion-hub /var/backups/minion-hub
sudo install -m 0600 -o minion-hub -g minion-hub ~/minion-backup.pass /etc/minion-hub/backup.pass
sudo -u minion-hub env MINION_DATA_DIR=/var/lib/minion-hub/data \
  minion hub backup --passphrase-file /etc/minion-hub/backup.pass \
  --out /var/backups/minion-hub/minion-hub-$(date +%F).age
```

A passphrase file that lives next to the archive it protects adds nothing; keep it where the backup job can read it and the copies of the archives cannot reach it, and keep another copy of the passphrase elsewhere.

The data directory is `MINION_DATA_DIR` unless you give `--data-dir`. The archive is owned by `minion-hub` with mode 0600. It is already encrypted, so it can be copied off the machine; do not copy an `--unencrypted` archive to a shared place.

**Docker Hub** ([`deploy/compose/hub.yaml`](../../deploy/compose/hub.yaml)): run the same command in a one-off container of the Hub service. The container runs as UID 65532 and writes into the data directory it has mounted:

```sh
sudo install -m 0600 -o 65532 -g 65532 ~/minion-backup.pass ./hub-data/backup.pass
docker compose -f hub.yaml run --rm --no-deps hub hub backup --passphrase-file /data/backup.pass --out /data/backup-$(date +%F).age
sudo rm ./hub-data/backup.pass
sudo install -m 0600 -o root -g root ./hub-data/backup-*.age /secure/place/
sudo rm ./hub-data/backup-*.age
```

The passphrase file must belong to the Hub's user (UID 65532) because the command checks the owner, and it should not stay in the data directory. The archive is created by that user with mode 0600 in the data directory, so reading it from the host needs `sudo`, and it should not stay there either. Replace `/secure/place/` by a directory only administrators can read.

Back up before every update you may want to undo: the Hub upgrades its database when it starts and there is **no database downgrade**.

## Restore

Restore into a stopped Hub. `minion hub restore` recognises an age-encrypted archive by its header; give the passphrase with `--passphrase-file FILE` (same rules as for the backup). It also restores a **plain** archive from `minion` 0.5 or from `--unencrypted` without any flag. The whole archive is decrypted, unpacked and checked in a private temporary directory outside the data directory before anything there is touched or even created: a wrong passphrase, a damaged or truncated file, a bad checksum or gzip trailer, or an archive whose database records a Hub identity without the matching `identity.key` fails with an error, also with `--force`, and leaves the data directory as it was (a data directory that did not exist still does not exist). The data directory is created and filled only after every check passed. Restore also refuses an archive that asks for an unusually expensive passphrase derivation, declares impossible sizes for the small key files, or exceeds `--max-size` (bytes, default 64 GiB; it limits the decrypted size and the total size of the files); the temporary directory is on the system temporary file system (`TMPDIR`), so it needs room for the archive and the database. A data directory that already holds files is refused unless you give `--force`, which replaces the database, the data key, the identity key and the generated TLS material and leaves other files alone. An empty data directory needs no `--force`. An archive written by a newer `minion` (a newer schema) is refused.

Restored files are created with mode 0600 and belong to the user who ran the command, so run the restore as the user that runs the Hub, and let that user read the archive.

**Native Hub:**

```sh
sudo systemctl stop minion-hub
sudo install -m 0400 -o minion-hub -g minion-hub /secure/place/minion-hub-2026-01-31.age /var/backups/minion-hub/restore.age
sudo -u minion-hub env MINION_DATA_DIR=/var/lib/minion-hub/data \
  minion hub restore --passphrase-file /etc/minion-hub/backup.pass /var/backups/minion-hub/restore.age
# add --force when the data directory already holds files
sudo rm /var/backups/minion-hub/restore.age
sudo systemctl start minion-hub
```

**Docker Hub:** restore in a one-off container, which runs as UID 65532 and so creates the files with the owner the Hub needs. Give that user a private copy of the archive for the run:

```sh
docker compose -f hub.yaml stop hub
sudo install -d -m 0700 -o 65532 -g 65532 ./restore
sudo install -m 0400 -o 65532 -g 65532 /secure/place/minion-hub-2026-01-31.age ./restore/backup.age
sudo install -m 0400 -o 65532 -g 65532 ~/minion-backup.pass ./restore/backup.pass
docker compose -f hub.yaml run --rm --no-deps -v "$PWD/restore:/restore:ro" hub hub restore --passphrase-file /restore/backup.pass /restore/backup.age
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

## If a backup is disclosed

An encrypted archive protects the file at rest. Anyone who learns the passphrase, and anyone who holds an unencrypted archive, holds the **Hub identity key** and the data key. That is a Hub identity compromise: the holder can impersonate the Hub to every Worker that pins it. A compromised identity key is not rotated through the signed chain of planned rotations. The Hub gets a **new identity** and the Workers are **enrolled again**:

1. Stop the Hub and set aside the compromised data directory (database, data key, identity key and TLS material), or start the Hub with a new empty data directory. A new identity key is generated on the first start; `minion hub identity` prints the new fingerprint.
2. On each Worker machine, stop the Worker, run `minion worker reset`, and enroll it again with a new invitation key or pairing code, which pins the new Hub identity. Approve each Worker as it arrives ([pairing](pairing.md#approve-a-new-worker)).
3. Treat the secrets stored in the old database as exposed (passwords, API keys, invitation keys, identity-provider client secret) and issue new ones.

This release has no mechanism that limits the effect of a disclosed backup.

## Moving the Hub to another machine

Back up on the old machine, install the same or a newer version on the new one ([binary](../install/hub-binary.md) or [Docker](../install/hub-docker.md)), restore into its empty data directory, start it, and give it the old name (DNS, or the address of your [reverse proxy](reverse-proxy.md)). Workers reconnect to that address. Stop the old Hub first: two Hubs with one identity answer Workers from two databases.

## What a restore does not bring back

- Sessions of browsers are in the database, so users stay signed in only until their session expires; passwords and API keys are in it too, so a restore from before a revocation **brings the revoked key back**. After restoring an old backup, review users, API keys and invitation keys and revoke what should not exist.
- The Hub's record of what each Worker shares is part of the database and is also declared again by each Worker when it connects. History that the Hub holds for a session stays hidden until the Worker's current sharing rule says it may be shown, whatever the backup held.
