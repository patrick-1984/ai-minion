# Run a Worker in Docker

Back to the [README](../../README.md).

Most Workers should run natively ([Linux](worker-linux.md), [macOS](worker-macos.md)): a Worker is a small program that talks to the tmux server of one user, and a container adds a boundary that the Worker then has to cross again. Use a container when you want the Worker's own files and its connection to the Hub kept apart from the rest of the host, or to run Workers from a Compose file.

The example [`deploy/compose/worker.yaml`](../../deploy/compose/worker.yaml) runs a Worker in a Debian container. The Hub image `ghcr.io/patrick-1984/minion` is **not** used for this: it has no tmux and no shell. The example installs `tmux` into a small Debian image and fetches the verified `minion` release with `bootstrap.sh` (see its header for the hosts it contacts).

## Two ways to use it

**A. Serve the sessions of a host user (the default in the example).** The container mounts that user's tmux socket directory and runs as that user's UID and GID. It then lists and controls the tmux sessions on the host. Requirements:

- A **Linux host**. Docker Desktop on macOS and Windows runs containers in a virtual machine; it cannot reach the tmux server of your Mac or PC. Run a native Worker there.
- The same **UID and GID** as the host user (`WORKER_UID`, `WORKER_GID` in the example). The tmux socket belongs to that user, and tmux refuses other users.
- The same **tmux version** in the image as on the host. A tmux client and a tmux server of different versions refuse each other. Compare `tmux -V` on the host with the image, and choose the base image to match.
- The socket path: tmux uses `/tmp/tmux-<uid>/default` unless `TMUX_TMPDIR` or `-S` says otherwise. The example mounts `/tmp/tmux-<uid>` and sets `MINION_TMUX_SOCKET=/tmux/default`.
- Stopping the container does not end the sessions: the tmux server runs on the host.

**B. A self-contained Worker.** Remove the socket mount and `MINION_TMUX_SOCKET` from the example. The Worker then starts its own tmux server inside the container and serves sessions that live in the container. Stopping or recreating the container ends them.

## The trust boundary

In mode A the container can do to those sessions whatever the host user can: read their screens, type into them, create sessions. It is the same power as a native Worker, with the container only keeping the rest of the host out of reach. It is **not** a sandbox against that user, and anything that shares the tmux socket shares that power. Never mount the Docker socket (`/var/run/docker.sock`) into a Worker container: that hands the container the whole host.

What the Hub may see of the sessions is still decided by the sharing choice ([pairing and sharing](../guides/pairing.md)); a Worker that shares everything shows the Hub every session of that user.

## Start it

1. Download `bootstrap.sh` from the release next to the compose file.
2. In the Hub: Workers, Add worker, Create an invitation key. The key is shown once, with commands. Save it in a file; a literal key on a command line is visible in the process list, so the example reads it from a file:

   ```sh
   printf '%s' 'mnk_...' > invite_key.txt
   ```

3. Start:

   ```sh
   export WORKER_UID=$(id -u) WORKER_GID=$(id -g)
   export MINION_SERVER=https://hub.example.com
   MINION_SHARE_EXISTING=later docker compose -f worker.yaml up -d --build
   docker compose -f worker.yaml logs worker
   ```

   A container has no terminal to ask on, so the first start needs `MINION_SHARE_EXISTING`: `all`, `none` or `later` (`later`: nothing is shared until you choose in the Hub, the safe default). **Give it on the first start only.** The Worker applies the value at every start while it is set, and the value replaces a choice you made in the Hub; start later with `docker compose -f worker.yaml up -d` and no `MINION_SHARE_EXISTING`, and the saved choice, the Hub's included, stays. The example builds its image from an inline Dockerfile, which needs a current Docker Compose (v2.17 or newer).

4. **Approve the Worker.** The first start enrolls the Worker, and a new Worker is **pending**: it connects, but the Hub sends it no operations until its owner or an administrator approves it. Open **Workers** in the Hub, compare the fingerprint, host name and source address with the container you started, and choose **Approve** (or run `minion hub worker approve <fingerprint>` with an administrator's API key). The Worker's log says `this Worker is waiting for approval` until then; after the approval it reconnects by itself. See [Approve a new Worker](../guides/pairing.md#approve-a-new-worker).

The `init` service runs once as root and gives the Worker's user the state directory, the cache and the key file. The Worker keeps its identity and database in `./worker-data` (`MINION_STATE_DIR=/data`); keep that directory, because the Worker's identity is in it. After the first start the Worker is enrolled and an enrolled Worker ignores the key. A key left in the file is still a credential until it expires or you revoke it in the Hub, but **do not delete the file**: Docker needs the mounted file to exist whenever the container is restarted or recreated, and a missing file stops the start. Empty it in place with the init service (an empty file is safe), or revoke the key on the invitations page:

   ```sh
   docker compose -f worker.yaml run --rm --no-deps init sh -c ': > /secret'
   ```

## Choosing what the Hub sees

Open the Worker's page in the Hub and use "Apply sharing", or call `share_set` through the Hub's API. See [pairing and sharing](../guides/pairing.md). `MINION_SHARE_EXISTING` replaces the saved choice at every start where it is set; leave it unset after the first start.

## Update

Change `MINION_VERSION` (an exact version such as `v0.6.2`, or a channel) and run `docker compose -f worker.yaml up -d` again. With an exact version you may add `MINION_SHA256`, the archive checksum from `checksums.txt` of that release. Without it the release signature is verified; with it only the archive checksum is checked and the signature is not, so take the checksum from a source you trust independently ([security](../security.md#updates)).
