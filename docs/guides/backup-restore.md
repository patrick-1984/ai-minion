# Back up and restore the Hub

Back to the [README](../../README.md).

The Hub's state is its **data directory** (`MINION_DATA_DIR`): the database (`minion.db`), the data key that protects secrets stored in it, the Hub **identity files** (the sealed root and the operational key, see [what a backup holds](#what-a-backup-holds-per-identity-state)), and the generated TLS certificate and key. Workers keep their own state; there is nothing of theirs to back up on the Hub side, and a Worker that reconnects to a restored Hub with the same identity does not need to pair again.

**The backup holds the Hub's secret keys.** A copy of the data key lets whoever holds it read what the Hub stores, and the identity files in it are the key material the Hub signs with (the operational key in plain form, the root sealed with the Hub passphrase, or, on a Hub that has no passphrase yet, the plain root). For that reason `minion hub backup` **encrypts the archive by default** (with [age](https://age-encryption.org), passphrase mode): you must pass `--passphrase-file FILE`, or `--unencrypted` to opt out on purpose. Keep the passphrase apart from the archive, and delete old archives. **Without the passphrase an encrypted backup cannot be restored; there is no recovery.**

## Back up

`minion hub backup` writes one archive with a consistent copy of the database (`VACUUM INTO`, safe while the Hub runs), the keys (data key, the Hub identity files of the Hub's current state, TLS key), the generated certificate and a manifest (format 2) with the version, the schema version, the identity state, the root fingerprint and the number of root transition records. The whole archive is a tar.gz that is then encrypted with age. It creates the file with mode 0600 and never overwrites an existing file.

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

Restore into a stopped Hub. `minion hub restore` recognises an age-encrypted archive by its header; give the passphrase with `--passphrase-file FILE` (same rules as for the backup). It also restores a **plain** archive from `minion` 0.5 or from `--unencrypted` without any flag. The whole archive is decrypted, unpacked and checked in a private temporary directory outside the data directory before anything there is touched or even created: a wrong passphrase, a damaged or truncated file, a bad checksum or gzip trailer, or an archive whose identity files do not fit the state recorded in its database (a recorded identity without its root file, a root file that is not the recorded one, an operational key that does not match the certificate, or a file the state does not have) fails with an error, also with `--force`, and leaves the data directory as it was (a data directory that did not exist still does not exist). The data directory is created and filled only after every check passed. Restore also refuses an archive that asks for an unusually expensive passphrase derivation, declares impossible sizes for the small key files, or exceeds `--max-size` (bytes, default 64 GiB; it limits the decrypted size and the total size of the files); the temporary directory is on the system temporary file system (`TMPDIR`), so it needs room for the archive and the database. A data directory that already holds files is refused unless you give `--force`, which replaces the database, the data key, the identity files and the generated TLS material and leaves other files alone. An empty data directory needs no `--force`. An archive written by a newer `minion` (a newer schema) is refused.

Restore works in two phases. First the archive is verified against its manifest in a private directory and never modified again; an entry the manifest does not list, or a name outside the fixed list of files, is refused. Then a copy is migrated to the schema of this binary (an archive from 0.6.x is classified by its migrated database and files), SQLite is checkpointed and closed (no `-wal` or `-shm` file), the integrity of the database is checked, and the identity files are validated against the state of the database. The files that are installed are checked once more against a plan computed from that transformed copy, so a migrated database is installed although its checksum differs from the manifest, and a file damaged between the check and the installation stops the restore and rolls back.

**A restore issues no certificate and asks for no passphrase.** The restored Hub is in the state of the backup. If the certificate has expired by then, or Workers refuse its serial, re-issue the certificate on the Hub identity page (or with the recovery command that the Worker logs).

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

After a restore, check that the identity is the one Workers pin. The identity files are private to the Hub's user, so run the command as that user: in the native install as `minion-hub`, in Docker in a container of the Hub service:

```sh
# native
sudo -u minion-hub env MINION_DATA_DIR=/var/lib/minion-hub/data minion hub identity
# Docker
docker compose -f hub.yaml run --rm --no-deps hub hub identity
```

It prints the fingerprint of the Hub root (52 lowercase base32 characters). Compare it with the one you recorded before, or with what `minion worker status` shows as the pinned Hub fingerprint on a Worker. The Hub keeps its root through restarts, certificate re-issues and restores of a backup that is not older than the last master-key rotation, so connected Workers reconnect on their own, without pairing again.

The root fingerprint is the one recorded in the Hub database, and the restore command prints it too ("Identity state: ... root <fingerprint>" for a sealed or established Hub). Compare the fingerprint that the restore prints with the one you recorded. For a Hub with a sealed root, `minion hub identity` shows the same value: it reads the recorded public root from the database (read-only), so it works although no `identity.key` exists there and the Hub need not be running.

## What a backup holds per identity state

The state is read from the copy of the database, never from which files happen to exist. The archive always holds the database, `data.key` and the TLS material, plus exactly the identity files of that state:

| State of the Hub | Identity files in the archive | Operational key |
| --- | --- | --- |
| No identity yet (fresh Hub, `none`) | none | none |
| Migrated from 0.6, no passphrase yet (`plain`) | the **plain** `identity.key` (the root is plain inside the archive; the command warns and the manifest says so) | none (there is no certificate yet) |
| Passphrase set, no certificate yet (`sealed_no_cert`) | the sealed root `root.key.age` | none |
| Established (`established`) | the sealed root `root.key.age` | `op.key` |

There is no mode that leaves a recorded root out. The age encryption of the archive is a second layer on top of the sealed root, not a replacement for it. Inside the archive the files always carry these names, whichever of `root.key.age` / `root.key.age.next` and `op.key` / `op.key.next` was the current one on disk: the backup chooses the sealed root by the SHA-256 that the database copy records, and the operational key by the public key of the certificate in the copy, and repeats the whole snapshot (at most five times) if a file of the state is not there because the state moved on between the copy and the read. If it still finds no matching file, the backup fails and names the state and the missing files; no archive is written.

An archive from `minion` 0.6.x (format 1) restores too: with no recorded identity and no key file it comes back as `none`; with a recorded identity and a matching plain `identity.key` as `plain` (set the passphrase afterwards); a recorded identity without its key file is refused.

## Which passphrase an archive needs

There are two passphrases, and they are not the same thing. The **archive passphrase** (`--passphrase-file`) only encrypts the archive file. The **Hub passphrase** opens the sealed root inside it and is asked only when you issue or re-issue a certificate or rotate the master key; a restore does not ask for it.

A backup holds the sealed root **of its own time**. After a passphrase change an older backup still opens with the **old** Hub passphrase; after a master-key rotation an older backup holds the retired root (next section). Keep the Hub passphrase of each backup generation in mind, or delete old backups.

## Root rollback: a backup older than a master-key rotation

A **serial rollback** (a backup from the same root with an older serial) is repaired by re-issuing the certificate with a serial floor. A **root rollback** is different: the backup was made **before a master-key rotation**, so it holds the retired root, while Workers have followed the rotation and pinned the new root. Workers only follow the chain of transitions forward, so a Worker pinned to the new root refuses the retired root, and re-issuing a certificate or raising the serial cannot repair that.

At restore time the command compares the manifest (root fingerprint, number of transition records) with the data directory it replaces. When the transition chain of that directory shows the root of the backup as the old root of a rotation (a longer chain alone proves nothing about this root), it prints "WARNING: this backup holds a root that may have been rotated away; Workers that followed the rotation will refuse this Hub". When it has no such evidence (a new, empty directory, or a directory whose chain does not leave the backup's root) it prints the same sentence as a conditional advisory with the backup date and the number of rotations in the backup, because it cannot know what happened after the backup: **you confirm the date**. Two ways back:

1. **Restore a backup made after the rotation.** It holds the new sealed root and the transition record.
2. If no such backup exists, the new root is lost with the data directory. The only way is **out-of-band re-pinning**: on every affected Worker run `minion worker reset` and enroll it again with a new invitation under the restored root (pairings and Worker identities are created again). A Worker that has not yet followed the rotation is not affected.

**Make a new backup after every master-key rotation, and do it first.**

## If a backup is disclosed

An encrypted archive protects the file at rest. Anyone who learns the archive passphrase, and anyone who holds an unencrypted archive, holds what the archive holds. What that is depends on the state (see the table above):

1. **The operational key** (`op.key`, established Hubs): whoever has it can impersonate the Hub to a Worker until the certificate expires. **Re-issue the certificate at once** (a new operational key, a higher serial): the thief's key stops working against every Worker that has seen the newer serial, but it stays usable against Workers that have not yet seen it, until the old certificate's `not_after`.
2. **The sealed root** (`root.key.age`): safe if the Hub passphrase is strong and out of the thief's reach. If the Hub passphrase is weak or may be known, treat it as a **root compromise** and plan the new identity: the Hub gets a new root and the Workers are enrolled again. **Changing the Hub passphrase does not help** against a thief who already holds the file, because the old file opens with the old passphrase; a master-key rotation protects only the future.
3. **A plain root** (`identity.key`, state `plain`, and every backup made by 0.6.x): once a disclosure is **known**, it is a **root compromise**. Sealing the root and rotating the master key do not help against a thief who already holds the plain key; follow the new-identity and Worker re-enrollment procedure below. Where no disclosure is known (for example right after the upgrade from 0.6.x), set the Hub passphrase and rotate the master key once as a precaution, so that the plain key in old backups and old copies stops being the current root.
4. The **data key** and what it protects are exposed in every case: treat the secrets stored in the database (passwords, API keys, invitation keys, identity-provider client secret) as exposed and issue new ones.

A compromised root is not rotated through the signed chain of planned rotations the thief could also sign. The Hub gets a **new identity** and the Workers are **enrolled again**:

1. Stop the Hub and set aside the compromised data directory, or start the Hub with a new empty data directory. Set the Hub passphrase to create the new root; `minion hub identity` prints the new fingerprint.
2. On each Worker machine, stop the Worker, run `minion worker reset`, and enroll it again with a new invitation key or pairing code, which pins the new Hub identity. Approve each Worker as it arrives ([pairing](pairing.md#approve-a-new-worker)).

## Moving the Hub to another machine

Back up on the old machine, install the same or a newer version on the new one ([binary](../install/hub-binary.md) or [Docker](../install/hub-docker.md)), restore into its empty data directory, start it, and give it the old name (DNS, or the address of your [reverse proxy](reverse-proxy.md)). Workers reconnect to that address. Stop the old Hub first: two Hubs with one identity answer Workers from two databases.

## What a restore does not bring back

- Sessions of browsers are in the database, so users stay signed in only until their session expires; passwords and API keys are in it too, so a restore from before a revocation **brings the revoked key back**. After restoring an old backup, review users, API keys and invitation keys and revoke what should not exist.
- The Hub's record of what each Worker shares is part of the database and is also declared again by each Worker when it connects. History that the Hub holds for a session stays hidden until the Worker's current sharing rule says it may be shown, whatever the backup held.
