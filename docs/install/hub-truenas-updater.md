# Hub updater for TrueNAS (host executor)

[Back to the README](../../README.md) · [Hub with Docker](hub-docker.md)

This page installs `minion hub-updater`, which keeps a Hub that runs as a **TrueNAS custom app** up to date without giving the Hub a NAS credential or the Docker socket. It runs **on the NAS host as root**, from a TrueNAS **Cron Task**, and it is the only privileged part. The Hub stays unprivileged.

Why not the in-Hub updater of [Hub with Docker](hub-docker.md#let-the-hub-update-itself-opt-in)? TrueNAS owns the compose project of a custom app and recreates every container from the YAML it stores, so a container replaced behind its back goes back to the old image at the next start, redeploy or edit. The host updater changes that stored YAML through the TrueNAS middleware itself and pins the Hub image by digest.

## What one run does

A run happens every 15 minutes. In this order:

1. **Lock.** One run per app (`flock`); an overlapping run exits at once.
2. **Reconcile.** If an earlier run was killed (cron timeout, reboot, power loss) the journal says where. The updater finishes the attempt or undoes it, even outside the window and when the Hub is down.
3. **Request.** It reads the request file (see below), if any.
4. **Admission.** Unattended attempts need `enabled: true` and the current UTC hour inside `window`; a valid one-shot request is admitted at any hour.
5. **Select and verify.** It resolves the channel with the same release selector and Sigstore verification as `minion update`, takes the image digest from the **signed release manifest** and requires a newer **stable** release whose signed protocol range contains the range of the running release. A digest that failed before is quarantined and not retried.
6. **Bind the images.** It pulls `repository@sha256:...` for the candidate (and the previous release when missing), maps both to local image ids and checks that the running Hub container runs the previous id.
7. **Transaction.** `app.stop`; wait until no container of the app runs; `zfs snapshot` of exactly the configured dataset, held; `app.update` with the full compose document and the Hub image changed to `repository@digest` while the app is stopped; `app.start`; sustained `/readyz` (60 s in a row within 5 min) from the container that runs the candidate image id; commit (snapshot released and destroyed).
8. **On failure:** `app.stop`, `zfs rollback` to the snapshot, `app.update` back to the saved compose document, `app.start`, verify that the previous image is ready; then the failed digest is quarantined. If the rollback itself fails (usually because a newer snapshot of the dataset exists) the held snapshot is kept, the app stays stopped and the status says `recovery_required` (see [recovery](#recovery)).

Changes written to the Hub between the start of the candidate and the commit are lost on a rollback: that is the price of a database that migrates forward only. Every wait is bounded (pull 15 min, middleware job 10 min, stop 3 min, readiness 5 min, whole run 45 min).

The compose document is sent to the middleware through the TrueNAS built-in Python client with the request on **stdin**; it never appears in a command line, and no value of it is logged or published. Errors are published as short codes only.

## Requirements

- TrueNAS SCALE 25.04 or newer (Docker apps), a custom app (installed with "Install via YAML") for the Hub. The stored document is read with `app.config` and written with `app.update` (`custom_compose_config`); this was designed against the middleware source and still needs a run on a scratch app of your TrueNAS release before you rely on it (see [Test it first](#test-it-first)).
- A **dedicated ZFS dataset** that holds exactly the Hub data directory (the app's `/data` bind mount) and nothing else. The updater snapshots and rolls back that dataset as a whole.
- Exclude that dataset from periodic snapshot tasks (or accept `recovery_required` when one runs during an attempt): a plain `zfs rollback` refuses to run when a newer snapshot exists, and the updater never uses `-r`.
- The Hub image reference in the app's YAML must be the Hub service you name in `service` (default `minion-hub`).
- The `docker`, `zfs` and `python3` commands of the TrueNAS host (all present).

## Install

Run as root on the NAS (shell or SSH). Use the release binary; check it as in [Hub as a binary](hub-binary.md). Put it where only root can replace it:

```sh
install -o root -g root -m 0755 minion /usr/local/bin/minion
```

The datasets and the directories (adjust the pool and names):

```sh
zfs create tank/apps/minion-hub-data                      # the Hub data (mounted at /mnt/tank/apps/minion-hub-data)
mkdir -p /mnt/tank/apps/minion-hub-exchange/in /mnt/tank/apps/minion-hub-exchange/out
```

The config file, owned by root and closed to others (flags of the same names override it):

```sh
install -o root -g root -m 0600 /dev/stdin /etc/minion-hub-updater.json <<'JSON'
{
  "app": "ai-minion",
  "service": "minion-hub",
  "dataset": "tank/apps/minion-hub-data",
  "readiness_url": "http://127.0.0.1:30080/readyz",
  "window": "03-05",
  "channel": "latest",
  "enabled": false,
  "state_dir": "/var/lib/minion-hub-updater",
  "request_file": "/mnt/tank/apps/minion-hub-exchange/in/request.json",
  "status_file": "/mnt/tank/apps/minion-hub-exchange/out/status.json"
}
JSON
```

| Field | Meaning |
| --- | --- |
| `app` | TrueNAS app name. |
| `service` | Compose service of the Hub in the app's YAML (default `minion-hub`). |
| `dataset` | ZFS dataset name (`pool/path`, **not** a `/mnt/...` path) that holds exactly the Hub data. |
| `readiness_url` | `/readyz` of the Hub as the host reaches it. Use `http://` or an `https://` URL the host accepts. |
| `window` | UTC hours `HH-HH` in which unattended attempts may start (`03-05` = 03:00 to 04:59; `22-02` wraps midnight). |
| `channel` | `latest`, `X` or `X.Y`. Never an exact version. |
| `enabled` | `true` allows unattended attempts in the window. `false` still reconciles and still honours a one-shot request. |
| `allow_hub_policy` | Default `false`. When `true`, the policy in the request file (written by the Hub) replaces `enabled`, `channel` and `window`. |
| `state_dir` | The journal and the Sigstore cache. Root-owned, mode 0700 (the updater refuses anything else). |
| `request_file`, `status_file` | The exchange with the Hub, below. Optional. |
| `min_free_bytes` | Free space the dataset needs before an attempt (default 1 GiB). |

Try it by hand first (nothing changes while `enabled` is `false` and no request exists):

```sh
/usr/local/bin/minion hub-updater --config /etc/minion-hub-updater.json
```

### The Cron Task

In the TrueNAS UI: **System > Advanced > Cron Jobs > Add**. Command `/usr/local/bin/minion hub-updater --config /etc/minion-hub-updater.json`, run as `root`, schedule every 15 minutes (`*/15 * * * *`), leave **Hide Standard Output** on, **Hide Standard Error** off. A run prints one line (`hub-updater: updated v0.17.0`), nothing sensitive.

## The exchange with the Hub

Two files in two directories. The Hub side is not part of this release; the format is fixed here.

| File | Written by | Read by | Rules |
| --- | --- | --- | --- |
| `request_file` (e.g. `.../exchange/in/request.json`) | the Hub, or the owner | the updater (as root) | The directory is the only one the Hub may write. The updater opens the file with `O_NOFOLLOW`, without blocking, requires a regular file of at most 16 KiB, strict JSON, `"version": 1`. Anything else is ignored and reported as `request_error` in the status. |
| `status_file` (e.g. `.../exchange/out/status.json`) | the updater, atomically | the Hub | The directory is mounted **read-only** into the Hub. The file holds codes, tags, digests and timestamps only. |

Request file, version 1 (all parts optional):

```json
{
  "version": 1,
  "policy": { "enabled": true, "channel": "latest", "window": "03-05", "revision": 3 },
  "once": {
    "id": "owner-2026-11-22",
    "baseline": "v0.15.0",
    "target": "v0.17.0",
    "policy_revision": 0,
    "expires_at": "2026-11-23T03:00:00Z"
  }
}
```

- `policy` is used only with `allow_hub_policy: true`.
- `once` updates from exactly `baseline` to exactly `target`, once. `id` is recorded as used in the journal **before** anything is disturbed; the same `id` is never run again. It needs an `expires_at` in the future and at most 7 days ahead. `baseline` must be the release the app runs now, proved against the signed manifest and the running container's image id; a mismatch refuses the attempt. `policy_revision`, when not 0, must equal the policy's `revision`.

The status file has the shape `{"version":1,"app","updated_at","tick","detail","current_tag","window","channel","enabled","quarantined":[...],"last_attempt":{"id","phase","reason","from_tag","to_tag","digest","terminal","outcome","needs_human"},"request_error","request_handled"}`. `tick` is one of `idle`, `disabled`, `outside_window`, `hub_down`, `up_to_date`, `updated`, `rolled_back`, `reconciled`, `reconcile_pending`, `recovery_required`, `refused` (with a reason code in `detail`), `busy`.

## First upgrade of a legacy app (exact tag)

An app installed with `ghcr.io/patrick-1984/minion:v0.16.0` (an exact tag) runs a Hub that exports no policy and no request yet. Authorize one upgrade as the owner, on the host, with an expiring request bound to what runs now and what you want:

```sh
cat > /mnt/tank/apps/minion-hub-exchange/in/request.json <<'JSON'
{"version":1,"once":{"id":"owner-first-upgrade","baseline":"v0.16.0","target":"v0.17.0","expires_at":"2026-11-23T03:00:00Z"}}
JSON
```

The next Cron tick (or a manual run) verifies both releases, checks the running image id, and runs the transaction above. The stored YAML then names `repository@sha256:...`, so a later TrueNAS redeploy or reboot keeps the new Hub. Remove the file afterwards. Arm unattended updates by setting `"enabled": true` in the config (or, later, from the Hub's Updates page).

## Test it first

Before you arm the updater on your real Hub, run the whole cycle on a **scratch custom app of the same TrueNAS release** with its own small dataset: a normal upgrade (then redeploy the app from the TrueNAS UI and check that the new image survives), and a forced failure after a migration (for example by pointing the candidate at an image whose `/readyz` never answers) to see the old image and the old data return. The unit tests prove the order of the steps with fakes; they cannot prove the behaviour of your middleware.

## Recovery

Look at the state first:

```sh
cat /mnt/tank/apps/minion-hub-exchange/out/status.json
ls /var/lib/minion-hub-updater/apps/ai-minion/       # attempt.json state.json compose-before.json
zfs list -t snapshot -o name,creation tank/apps/minion-hub-data
zfs holds tank/apps/minion-hub-data@<snapshot>
```

- `tick: reconcile_pending`: a middleware job of an interrupted run still runs, or a restore step failed and will be retried. Do nothing; the next tick continues.
- `tick: recovery_required` (the app is usually stopped): the updater did everything safe and stopped. `reason` says why: `rollback_failed` (a newer snapshot exists), `snapshot_missing`, `saved_compose_missing`, `previous_not_ready`. Restore by hand:
  1. `midclt call -job app.stop <app>` (the app must be stopped).
  2. Roll the dataset back: `zfs rollback -r tank/apps/minion-hub-data@<snapshot>` (this destroys newer snapshots; decide that yourself), then `zfs release minion-hub-updater <dataset>@<snapshot>`.
  3. Put the previous image back: in the TrueNAS UI edit the app YAML to the previous reference (`attempt.json` has `previous_ref`), or update it from the saved compose in `compose-before.json` (it holds your configuration, readable by root only).
  4. Start the app and check the Hub.
  5. Tell the updater: `minion hub-updater --config /etc/minion-hub-updater.json --resolve-recovery`.
- A digest that failed is quarantined and not retried. When a fixed release has a new digest it is picked up by itself. To retry the same digest: `minion hub-updater --config ... --clear-quarantine`.
- A leftover snapshot named `minion-pre-<version>-<id>` after a finished attempt is removed by the next tick; destroy it by hand (`zfs release` first) if that does not happen.
- To stop the updater, set `"enabled": false` (it still reconciles) or disable the Cron Task. The updater never touches the app outside an attempt.
