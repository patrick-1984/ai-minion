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
6. **Bind the images and the data.** It pulls `repository@sha256:...` for the candidate (and the previous release when missing), maps both to local image ids and checks that the running Hub container runs the previous id. It also resolves the host source of the container's data mount and requires that it is exactly the configured dataset (its name and GUID are recorded and checked again before the snapshot and the rollback). The policy window and a one-shot request are checked once more immediately before the attempt is recorded.
7. **Transaction.** `app.stop`; wait until no container of the app runs; `zfs snapshot` of exactly the configured dataset, held; `app.update` with the full compose document, the Hub image changed to `repository@digest` and `MINION_MAINTENANCE=readonly` added, while the app is stopped; `app.start`; sustained `/readyz` (60 s in a row within 5 min) asked of the candidate container's own address, restarting the timer when the container is replaced; then `app.update` without the read-only flag (the middleware redeploys the app), 20 s of sustained readiness again, and commit (snapshot released and destroyed).

While `MINION_MAINTENANCE=readonly` is set the Hub answers 503 to every request that could change state (any method but GET, HEAD and OPTIONS, and every WebSocket upgrade, so Workers wait and retry). The trial therefore cannot acknowledge a write that a later rollback would erase. The window after the flag is lifted is short (20 s) but writes accepted in it are lost if that last check fails.
8. **On failure:** (after every middleware job of the attempt has ended) `app.stop`, `zfs rollback` to the snapshot, `app.update` back to the saved compose document with the previous image **pinned by the verified digest** (never a tag), `app.start`, verify that the previous image id is ready; then the failed digest is quarantined. If an administrator edited the app meanwhile, nothing is overwritten: the state is `recovery_required` (`compose_changed_by_admin`). If the rollback itself fails (usually because a newer snapshot of the dataset exists) the held snapshot is kept, the app stays stopped and the status says `recovery_required` (see [recovery](#recovery)).

The database migrates forward only, so the rollback restores the snapshot taken before the candidate started. Every wait and every external command is bounded (one call 30 s, pull 15 min, middleware job 10 min, stop 3 min, readiness 5 min, whole restore 30 min, whole run 45 min).

The compose document is sent to the middleware through the TrueNAS built-in Python client with the request on **stdin**; it never appears in a command line, and no value of it is logged or published. Errors are published as short codes only.

## Requirements

- TrueNAS SCALE 25.04 or newer (Docker apps), a custom app (installed with "Install via YAML") for the Hub. The stored document is read with `app.config` (which returns the compose document itself) and written with `app.update` (`custom_compose_config`); the updater refuses an app that `app.query` does not report as custom. Prove it on a scratch app of your TrueNAS release before you rely on it (see [Test it first](#test-it-first)).
- A Hub release that knows `MINION_MAINTENANCE=readonly` (this release and later). Until the Hub you run has it, the trial is not write-fenced: keep unattended updates off (`enabled: false`) and run one-shot upgrades only while no one uses the Hub, or send traffic away from it.
- A **dedicated ZFS dataset** that holds exactly the Hub data directory (the app's `/data` bind mount) and nothing else. The updater snapshots and rolls back that dataset as a whole.
- Exclude that dataset from periodic snapshot tasks (or accept `recovery_required` when one runs during an attempt): a plain `zfs rollback` refuses to run when a newer snapshot exists, and the updater never uses `-r`.
- The Hub image reference in the app's YAML must be the Hub service you name in `service` (default `minion-hub`).
- The `docker`, `zfs` and `python3` commands of the TrueNAS host (all present).

## Install

TrueNAS keeps `/usr` read-only and does not carry `/etc` or `/var/lib` across a boot-environment upgrade, so everything of the updater lives on a **persistent pool dataset of its own**, outside the Hub data (the Hub rollback must never touch it). Run as root on the NAS; adjust the pool and names:

```sh
zfs create tank/minion-hub-updater                  # mounted at /mnt/tank/minion-hub-updater
chown root:root /mnt/tank/minion-hub-updater && chmod 0700 /mnt/tank/minion-hub-updater
install -o root -g root -m 0755 minion /mnt/tank/minion-hub-updater/minion   # check the binary as in hub-binary.md
zfs create tank/apps/minion-hub-data                # the Hub data (the app's /data bind mount)
mkdir -p /mnt/tank/apps/minion-hub-exchange/in /mnt/tank/apps/minion-hub-exchange/out
```

The updater refuses a state directory when it or any ancestor is not owned by root or is writable by group or others. Create the Hub exchange directories outside it (the Hub writes `in`, reads `out`).

The config file, owned by root and closed to others (flags of the same names override it):

```sh
install -o root -g root -m 0600 /dev/stdin /mnt/tank/minion-hub-updater/config.json <<'JSON'
{
  "app": "ai-minion",
  "service": "minion-hub",
  "dataset": "tank/apps/minion-hub-data",
  "data_mount": "/data",
  "window": "03-05",
  "channel": "latest",
  "enabled": false,
  "state_dir": "/mnt/tank/minion-hub-updater/state",
  "request_file": "/mnt/tank/apps/minion-hub-exchange/in/request.json",
  "status_file": "/mnt/tank/apps/minion-hub-exchange/out/status.json"
}
JSON
```

| Field | Meaning |
| --- | --- |
| `app` | TrueNAS app name. |
| `service` | Compose service of the Hub in the app's YAML (default `minion-hub`). |
| `dataset` | ZFS dataset name (`pool/path`, **not** a `/mnt/...` path) that holds exactly the Hub data. It must be the source of the container's `data_mount` bind mount, or the updater refuses. |
| `data_mount` | The Hub data path inside the container (default `/data`). |
| `window` | UTC hours `HH-HH` in which unattended attempts may start (`03-05` = 03:00 to 04:59; `22-02` wraps midnight). |
| `channel` | `latest`, `X` or `X.Y`. Never an exact version. |
| `enabled` | `true` allows unattended attempts in the window. `false` still reconciles and still honours a one-shot request. |
| `allow_hub_policy` | Default `false`. When `true`, the policy in the request file (written by the Hub) replaces `enabled`, `channel` and `window`. |
| `state_dir` | Required. The journal and the Sigstore cache, on the persistent dataset above. |
| `request_file`, `status_file` | The exchange with the Hub, below. Optional. |
| `min_free_bytes` | Free space the dataset needs before an attempt (default 1 GiB). |

The Hub is probed at the container's own address and port (from `MINION_LISTEN` and `MINION_TLS` in the app's YAML, default `https` on 8443), never through a published port or a proxy, so another Hub cannot answer for it.

Try it by hand first (nothing changes while `enabled` is `false` and no request exists):

```sh
/mnt/tank/minion-hub-updater/minion hub-updater --config /mnt/tank/minion-hub-updater/config.json
```

### The Cron Task

In the TrueNAS UI: **System > Advanced > Cron Jobs > Add**. Command `/mnt/tank/minion-hub-updater/minion hub-updater --config /mnt/tank/minion-hub-updater/config.json`, run as `root`, schedule every 15 minutes (`*/15 * * * *`), leave **Hide Standard Error** off. The command exits non-zero with one sanitized line on stderr when an update needs recovery by hand, so the Cron Task reports a failure instead of staying silent. A run prints one line on stdout (`hub-updater: updated v0.17.0`), nothing sensitive. After a NAS upgrade, check once that the Cron Task still runs.

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
ls /mnt/tank/minion-hub-updater/state/apps/ai-minion/       # attempt.json state.json compose-before.json
zfs list -t snapshot -o name,creation tank/apps/minion-hub-data
zfs holds tank/apps/minion-hub-data@<snapshot>
```

- `tick: reconcile_pending`: a middleware job of an interrupted run still runs, or a restore step failed and will be retried. Do nothing; the next tick continues.
- `tick: recovery_required` (the app is usually stopped; the Cron Task shows a failure): the updater did everything safe and stopped. `reason` says why: `rollback_failed` (a newer snapshot exists), `snapshot_missing`, `dataset_changed`, `saved_compose_missing`, `previous_not_ready`, `previous_image_changed`, `compose_changed_by_admin` (you edited the app during the attempt: nothing was overwritten). Restore by hand:
  1. Stop the app in the TrueNAS UI (Apps, the app, Stop) and confirm that no container runs: `docker ps --filter label=com.docker.compose.project=ix-<app>` lists nothing.
  2. If the data must go back (the candidate started: `candidate_started` is true in `attempt.json`), roll the dataset back: `zfs rollback -r tank/apps/minion-hub-data@<snapshot>` (this destroys newer snapshots; decide that yourself), then `zfs release minion-hub-updater tank/apps/minion-hub-data@<snapshot>`.
  3. Put the previous image back: edit the app YAML in the UI so the Hub image is `previous_ref` of `attempt.json` (a `repository@sha256:...` digest), and remove `MINION_MAINTENANCE` if present. `compose-before.json` holds the whole previous document (it contains your configuration, readable by root only).
  4. Start the app in the UI and check the Hub.
  5. Tell the updater: `... minion hub-updater --config ... --resolve-recovery`.
- `tick: reconcile_pending`: a middleware job of an interrupted run still runs, or a restore step failed and will be retried. Do nothing; the next tick continues.
- A digest that failed is quarantined and not retried. When a fixed release has a new digest it is picked up by itself. To retry the same digest: `minion hub-updater --config ... --clear-quarantine`.
- A leftover snapshot named `minion-pre-<version>-<id>` after a finished attempt is removed by the next tick; destroy it by hand (`zfs release` first) if that does not happen.
- To stop the updater, set `"enabled": false` (it still reconciles) or disable the Cron Task. The updater never touches the app outside an attempt.
