# Pairing a Worker and choosing what the Hub sees

Back to the [README](../../README.md).

A Worker runs alone (Solo) until you give it a Hub. Two things join it to a Hub: **pairing** (the Worker shows a code, you type it into the Hub) or an **invitation key** (the Hub gives you a key, the Worker enrolls itself with it). After that you choose which of the machine's sessions the Hub may see. Install the Worker first: [Linux](../install/worker-linux.md), [macOS](../install/worker-macos.md), [Docker](../install/worker-docker.md), [Windows preview](../install/worker-windows-preview.md).

## Pair with a code

On the machine that should become a Worker, as the Unix user whose sessions you serve:

```sh
minion worker pair --server https://hub.example.com
```

The Worker shows a code like `MN-K7Q4-XY12` and waits. In the Hub, open **Workers**, choose **Add worker**, type the code (capital or small letters, with or without hyphens) and, if you like, a name. The Hub and the Worker confirm each other with the code, and the Worker appears in your space as a **private Worker**: it belongs to you. It starts **pending**: see [Approve a new Worker](#approve-a-new-worker).

- The code is valid for 10 minutes; a fresh one is shown when it expires or the Hub rejects it. It is printed on the console and in the log, so keep those logs private. (The other deliberately printed secret is the Hub's generated first administrator password, see [security](../security.md#credentials).)
- The Worker does not look at the Hub's TLS certificate when it pairs: trust comes from the pairing handshake, and the Worker pins the Hub's identity key (its fingerprint is what `minion hub identity` prints; since 0.7.0 the pin is the Hub's root key, and the Hub proves it with a certificate, see [security](../security.md#two-keys-a-sealed-root-and-a-short-lived-operational-key)). A Worker can therefore pair through a proxy or a tunnel whose certificate it does not trust.
- `minion worker join https://hub.example.com` is the same, and also keeps the address in the Worker's config file so that the Worker service connects to that Hub afterwards. A service that is already running keeps the address it started with: restart it.
- A paired Worker refuses with `already_paired`. `--new-code` creates a new Worker key, wipes the old enrollment, revokes the old identity at its Hub and pairs again, after a confirmation unless you give `--yes`.
- `minion worker setup` does the same inside its wizard.

The pairing handshake has not had an independent cryptographic review yet; see [security](../security.md).

### If pairing is interrupted

Pairing is **idempotent per pairing code and Worker key**. If the connection ends after the Hub accepted the code but before the Worker recorded the result (a lost answer, a crash of the Worker), the Worker keeps the code and the Hub identity it authenticated, and on the next start says `Resuming pairing: enter the same code again in the Hub (Add worker)`. Enter the same code again, as the same user who entered it the first time, before it expires (10 minutes). The Hub returns the Worker it already created (same id, name and approval state; audit `worker.pair_resume`) instead of failing with `already_paired`. Only a Worker that holds the same key can resume, a wrong code counts against five attempts, and a Worker that finds a different Hub identity answering refuses it.

If the 10 minutes have passed, the Hub has the Worker but the Worker has no pin. The owner (or an administrator) then opens the Worker from the **Workers** list and uses **Re-issue pairing** on its page (shown only while the Worker has never connected), or `minion hub worker repair-pairing <worker>` (the operation `worker_pairing_reissue`). It opens a **10-minute window** in which the Worker's next new code completes the pairing for the same Worker row. It is offered only for a Worker that has **never connected**; for any other Worker, or one that was removed, it answers `conflict`. Run `minion worker pair` on the Worker (or wait for its fresh code) and type the new code in the Hub. If the Worker's key itself is lost, remove the Worker in the Hub and enroll the new key. The Worker's own message for this case is: `The Hub already knows this Worker key. Ask the owner to use Re-issue pairing for this Worker.`

## Enroll with an invitation key

An invitation key lets a machine enroll itself, for example from a script or a Compose file. In the Hub open **Invitations** (or **Add worker**, then **Create an invitation key**) and choose:

- **mode**: `one_time` (enrolls exactly one Worker) or `reusable` (any number, until it expires or is revoked);
- **lifetime**: 1h, 4h, 8h, 24h, 1w, 1m or unlimited (expiry stops new enrollments only; a Worker that has enrolled is never affected). Only an administrator can create a key that is valid longer than **1 week**;
- **uses** (reusable keys): the number of Workers the key may enroll. A user's reusable key needs a limit: **10** when none is given, **100** at most. An administrator's reusable key without a limit stays unlimited (`--max-uses` on `invitation_create`);
- **kind**: `private` (the Worker belongs to the user who creates the key) or `infrastructure` (a shared Worker; administrators only).

The key, like `mnk_<fingerprint>.<secret>`, is shown **once**, with the commands to start the Worker. The same from the command line: `minion invitation_create --mode one_time --ttl 24h` against the Hub ([API keys and MCP](api-keys-and-mcp.md)). Revoke a key on the same page, or with `invitation_revoke`.

On the Worker:

```sh
printf '%s' 'mnk_...' > invite_key
chmod 0600 invite_key
minion worker up --server https://hub.example.com --invite-key ./invite_key --share-existing later
```

Prefer the **file**. `--invite-key` takes the key itself, a file that holds it, or `-` to read it from standard input, and the environment form is `MINION_INVITE_KEY_FILE` (a file; `MINION_INVITE_KEY` holds the key itself). A key given literally on the command line is **visible in the process list** of the machine. The file must be owned by the user who runs the Worker and closed to everyone else. A key left on disk is a credential until it expires or you revoke it: delete the file once the Worker is enrolled (for a Worker in Docker, empty it in place instead, as [its guide](../install/worker-docker.md) shows), or revoke the key.

The key carries the Hub's identity fingerprint, so a Worker that enrolls with it pins the right Hub without trusting any TLS certificate.

A key does not make a Worker usable by itself: a Worker that enrolls with it is **pending** until you approve it, so a stolen or leaked key cannot operate anything.

### Approve a new Worker

Every new Worker starts **pending**, whichever way it joined: pairing code, invitation key or the Hub-wide infrastructure key. A pending Worker may connect, and the Hub shows it as such, but the Hub routes no operation to it (it cannot be a target of any user, key or other Worker), and the calls it makes to the Hub are refused with `worker_pending`. The one thing it may still do is retire itself (`minion worker reset` on its machine, or removing it in the Hub).

The owner of the Worker, or an administrator, approves it:

- **In the Hub:** the **Workers** page shows a *pending approval* badge and a banner. Each pending Worker shows its **fingerprint** (the fingerprint of its key), the **host name** it announced and the **source address** it enrolled from; compare them with the machine you just started, then choose **Approve**, or **Remove** if you do not recognise it. The Worker reconnects within moments and then receives operations.
- **From the command line:** `minion hub worker approve <fingerprint|id>` with `MINION_HUB_URL` and `MINION_API_KEY_FILE` set. The argument is a Worker **id** or a **fingerprint**, and the two need different permissions. An id is approved directly and needs only *manage* (or *operate*) on that Worker, so an administrator's API key with *manage* alone can approve exactly the Worker that automation started, if it knows its id. A fingerprint is first looked up in the Workers list, so that form also needs *read* on the Worker. `minion workers_list --all` shows ids and fingerprints.
- **Through the API:** the operation `worker_approve` (`worker_id`, and optionally the `fingerprint` you looked at; the Hub approves only if it matches). It needs *operate* or *manage* on the Worker; a Worker identity, and an API key bound to that very Worker, can never approve it.

The Worker's log says `this Worker is waiting for approval` while it waits. Both the enrollment and the approval are in the audit log (`worker.enroll` with outcome `pending`, then `worker.approve`).

Workers that existed when you upgraded to 0.6 were approved by the upgrade. Invitation keys of non-admin users that had no expiry now expire 7 days after the upgrade, and reusable keys without a limit allow 10 more enrollments; the Invitations page shows a notice on each key that was limited.

### The infrastructure key file

`MINION_INFRA_INVITE_KEY_FILE` names a file with one Hub-wide key that enrolls infrastructure Workers (owned by `infra_default_owner`, or the first administrator). The Hub keeps it as a row of the invitation list, keyed by the hash of its secret, with an **expiry** (`infra_invite_key_days`, default 30 days from the first load; `0` means none and the Hub warns at every start). The setting applies when the Hub first records a key: changing it later does **not** change the expiry of an existing key. To change the lifetime, set `infra_invite_key_days` **and** rotate the secret in the key file (or revoke the existing key), a **use counter** and a **revocation** that sticks. Restarting the Hub with the same file never resets the expiry or the counter and never undoes a revocation. Administrators see the row in **Invitations** and revoke it there (or with `invitation_revoke`) like any key; to rotate, put a new key in the file. Workers it enrolls are pending like all others. The key is bound to the Hub identity, so a Hub that has no root key yet (a new Hub in the state B0, before the passphrase is set) **refuses to start** while `MINION_INFRA_INVITE_KEY_FILE` is set, and its message tells you to unset it, start the Hub, set the Hub passphrase and set the variable again ([Hub identity](hub-identity.md)). A Hub migrated from 0.6 already has its root and starts with the file.

## Choosing what the Hub sees

The Worker decides which of its tmux sessions the Hub may see. There are four settings:

| Choice | Meaning |
| --- | --- |
| `all` | every session, also the ones you create later |
| `select` | exactly the sessions you name |
| `none` | no session |
| `later` | nothing is shared until you choose (the first-run state when you chose "decide later") |

A session that is not shared is **listed by name only** to its owner and the administrators, so that they can choose it. The Hub never shows its content, its task, its state or its history.

Ways to choose (any of them; the last change wins):

- **On the machine:** `minion worker config share all|none|later`, or `minion worker config share select <session>...`, or the setup wizard. The change takes effect at once on the Worker and reaches the Hub within the Worker's poll interval (one second by default, `session_poll_interval`).
- **In the Hub:** open **Workers**, then the Worker's page, choose "All sessions, also new ones", "Only the sessions I choose" or "No session", tick the sessions for the middle choice, and press **Apply sharing**. This is the `share_set` operation; you need the *operate* right on the Worker (the owner and administrators have it; an API key needs it in its scope). Through the API or the command line: `minion share_get --worker-id <id>` and `minion share_set --worker-id <id> --mode select --names work --names build`, with `MINION_HUB_URL` and `MINION_API_KEY_FILE` set ([API keys and MCP](api-keys-and-mcp.md)). Both name the Worker in `--worker-id`, which is required; without a Hub the command is refused rather than sent to the local Worker.
- **Lock it locally:** start the Worker with `--share-lock` (or `MINION_SHARE_LOCK=true`) and the Hub can no longer change the choice: `share_set` is refused. You still change it on the machine. A Worker that is **preview-only** (`--preview-only`, or restricted by the Hub) refuses `share_set` too.

### What the Hub does when a session stops being shared

- It stops showing the session's content at once: snapshots, captures, events and the stored **pipe history** are refused with `not_shared`, also for a reader that was already connected. History is hidden, not erased: if you share the session again, what was stored shows again.
- The Hub keeps each Worker's share rule durably, with a revision that rises with every change, and it hides history by the rule, not by which sessions the Worker happens to list. A restart of the Hub, a reconnect of the Worker, a closed session or an answer that does not mention the session cannot make it visible again.
- A change that was started and whose answer was lost (the connection ended before the Worker answered) returns the error `connection_lost` and may or may not have been made. The Hub then hides what the change would have hidden until the Worker has confirmed its rule, and asks it. Ask for the current state with `share_get` before you repeat the change; repeating it is harmless.
- While the Hub does not know a Worker's rule it hides that Worker's stored history: a Worker whose connection has not yet declared its rule, and a Worker of an older version that declares none. Upgrade such Workers. After a Hub upgrade, the history of Workers that are offline is hidden until they reconnect once.
- `share_get` and `share_set` act only on the Worker that runs them: a Worker refuses a `worker_id` that is not its own, and a Worker with no Hub identity refuses them.

### Who decides at start

`--share-existing all|none|later` (or `MINION_SHARE_EXISTING`, or `share_existing` in the config file) is the operator's explicit word, and the Worker applies it **at every start**, replacing the saved choice, including one made in the Hub. When the value is empty, the saved choice stays, and a choice made in the Hub or with `minion worker config share` survives restarts.

- `minion worker up` (as a service) saves the flag in the config file, so it applies at every restart. To hand the choice to the Hub after the first run, run `minion worker config unset share_existing`.
- `minion worker config share ...` keeps the config file in step, so it never gets undone by a start.
- A foreground Worker or a container applies the value only while it is set: give `MINION_SHARE_EXISTING` on the first start and leave it out afterwards.

## After pairing

- **Workers** in the Hub lists the Worker with its owner, kind and whether it is online, or pending approval. `minion worker status` shows the same from the Worker's side: the connection state, the pinned Hub fingerprint and the share choice.
- To end a pairing, remove the Worker in the Hub: it becomes a tombstone, its trust and bound API keys are deleted, and the Worker is told to wipe its pairing. From the machine's side, `minion worker reset` (the Hub is not told: remove the Worker in the Hub as well).
- Hub administrators can rename, reassign and restrict Workers (to preview only, which refuses every write to a session while it is on).
