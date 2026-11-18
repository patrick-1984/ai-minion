# Pairing a Worker and choosing what the Hub sees

Back to the [README](../../README.md).

A Worker runs alone (Solo) until you give it a Hub. Two things join it to a Hub: **pairing** (the Worker shows a code, you type it into the Hub) or an **invitation key** (the Hub gives you a key, the Worker enrolls itself with it). After that you choose which of the machine's sessions the Hub may see. Install the Worker first: [Linux](../install/worker-linux.md), [macOS](../install/worker-macos.md), [Docker](../install/worker-docker.md), [Windows preview](../install/worker-windows-preview.md).

## Pair with a code

On the machine that should become a Worker, as the Unix user whose sessions you serve:

```sh
minion worker pair --server https://hub.example.com --hub-fingerprint <the 52 characters from the Hub host>
```

In a terminal without `--hub-fingerprint`, the Worker first asks for the Hub's fingerprint ([give the Worker the Hub fingerprint](#give-the-worker-the-hub-fingerprint)). Then it shows a code like `MN-K7Q4-XY12` and waits. A Worker that runs (`minion worker run`, `minion worker up`, the service) with a Hub address, no pairing and no invitation key does the same by itself: it prints the code on the console and in the log, and connects as soon as the code is entered, without a restart. In the Hub, open **Workers**, choose **Add worker**, type the code (capital or small letters, with or without hyphens) and, if you like, a name. The Hub and the Worker confirm each other with the code, and the Worker appears in your space as a **private Worker**: it belongs to you. It starts **pending**: see [Approve a new Worker](#approve-a-new-worker).

- The code box shows the **Hub fingerprint** (six words and 52 characters) of the Hub that answered, "confirmed by the pairing code: the Hub shows the same words on Workers > Add worker", and the **Worker fingerprint**. The code itself confirms the Hub: the code is the password of the CPace exchange, whose session id is the hash of both hellos, and the Hub's hello carries its key, so the code entered in your Hub completes only with your Hub's key. Compare the words with the Add worker page if you like; nothing has to be restarted. A Hub older than 0.12.0 answers only after the code was entered, so the box then has no Hub line.
- The code is valid for 10 minutes; a fresh one is shown when it expires or the Hub rejects it. It is printed on the console and in the log, so keep those logs private. (The other deliberately printed secret is the Hub's generated first administrator password, see [security](../security.md#credentials).)
- The Worker does not rely on the Hub's TLS certificate when it pairs: it checks the Hub's fingerprint, and it pins the Hub's identity key (since 0.7.0 the pin is the Hub's root key, and the Hub proves it with a certificate, see [security](../security.md#two-keys-a-sealed-root-and-a-short-lived-operational-key)). A Worker can therefore pair through a proxy or a tunnel whose certificate it does not trust.
- `minion worker join https://hub.example.com --hub-fingerprint <52 characters>` is the same, and also keeps the address and the fingerprint in the Worker's config file so that the Worker service connects to that Hub afterwards. A service that is already running keeps the address it started with: restart it.
- A paired Worker refuses with `already_paired`. `--new-code` creates a new Worker key, wipes the old enrollment, revokes the old identity at its Hub and pairs again, after a confirmation unless you give `--yes`.
- `minion worker setup` does the same inside its wizard.

The pairing handshake has not had an independent cryptographic review yet; see [security](../security.md).

### Give the Worker the Hub fingerprint

Since 0.11.0 a Worker can be told which Hub it may pair with. It then refuses any other Hub before the code is used, whatever answers at the address and whatever certificate it shows. Read the fingerprint **on the Hub host**, not on a web page:

```sh
minion hub identity --words
docker exec <hub container> minion hub identity --words
```

It prints six words (the **Hub fingerprint**), the 52 characters (the full fingerprint) and ready lines for a Worker. The six words are words from the BIP39 English word list (not a BIP39 mnemonic) for the first 66 bits of the fingerprint. A Hub before 0.11 has no `--words`: use the 52 characters that `minion hub identity` prints.

Where a fingerprint is asked, give six words, separated by `-`, spaces or commas, in any letter case, or the 52 characters. The first four letters of each word are enough; a shorter token must be a whole word. The Worker always compares all six words. For copying and automation use the 52 characters.

- **The setting** `hub_fingerprint`: `--hub-fingerprint`, `MINION_HUB_FINGERPRINT` or the config file. A malformed value stops the Worker from starting. `minion worker join` and the setup wizard save it together with the Hub address.
- **The prompt.** `minion worker pair`, `join`, `setup` and the `up` wizard in a terminal without the setting show "NEW HUB - ENTER ITS FINGERPRINT", before any network check and before anything is changed. Enter alone asks again; `skip` pairs without the check; the end of input aborts.
- **Unattended** (a service or a container without the setting): the Worker still pairs on first use, as before; the code confirms the Hub it is entered in, and `minion worker status` and the Hub show the Hub check as `not verified` (nobody compared the key with the Hub host).

If another Hub answers, the Worker prints "HUB FINGERPRINT DOES NOT MATCH - NOT PAIRED" with the expected and the answering words, stores nothing and ends (exit status 1). Either the value or the address is wrong, or something in between answers in the Hub's place. If you already typed the code in the Hub, remove the pending Worker there that does not show this Worker's fingerprint.

When pairing succeeds, the box "MINION WORKER PAIRED" shows, in this order: the **Owner** ("Not your account? Run: minion worker reset"), the Hub fingerprint as words and 52 characters with "It matched the fingerprint you gave.", "Confirmed by the pairing code ..." or "Admitted on the Hub without the code ...", the [TLS path](reverse-proxy.md#what-a-worker-reports-behind-a-proxy), and the Worker fingerprint as words and 52 characters. Check the owner line: a correct Hub fingerprint does not prove which account claimed the Worker.

An older Worker treats `--hub-fingerprint` as an unknown flag and ignores `MINION_HUB_FINGERPRINT` without a word.

### Check a paired Worker: `verify-hub`

```sh
minion worker verify-hub <the 52 characters, or the six words>
```

It needs no running Worker. If the value matches the paired Hub, the Hub is recorded as checked. If it does not match, the mismatch is stored (a quarantine): the command prints "HUB FINGERPRINT DOES NOT MATCH - NOT CONNECTING" and exits with status 1, and the Worker does not connect to that Hub, also after restarts. A running Worker of v0.11.0 or later drops its connection within its next poll; the command says so only once that Worker confirmed it, and otherwise tells you to check `minion worker status`. A running older Worker cannot apply the check: the command then changes nothing and asks you to stop it first (`minion worker down`). A Worker that is not paired exits with status 1.

The quarantine ends only with `verify-hub <the 52 characters>` of the paired key (six words are not enough here) or with `--forget` below. The setting `hub_fingerprint` never ends it, also when it names the paired key.

On a paired Worker the setting `hub_fingerprint` is compared in the same way at every start. A value that names the key this Worker was checked with, or any key the Hub moved through since by verified signed master-key rotations, still connects and logs a warning to update the setting.

To give up a disputed pairing, remove it (like `ssh-keygen -R`) and pair again:

```sh
minion worker verify-hub --forget --pinned <the 52 characters of the pinned key>
```

`--pinned` must name the pinned key exactly (`minion worker status` shows it). The Worker never trusts that key again by itself: the removal is recorded in the state directory, `invite_key` is removed from the config file, and a start does not enroll again from an invitation key that is still configured (a mounted `MINION_INVITE_KEY_FILE`, `--invite-key`); it stops with a message instead. Pair again with `minion worker pair --hub-fingerprint <value>`, or run `minion worker reset` before you enroll with a new invitation key.

### Admit a waiting Worker: Pairing requests

Since 0.12.0 an administrator can admit a waiting Worker without typing its code, as NetBird approves a new peer. A Worker that waits with a code appears under **Pairing requests** on the **Workers** page and on **Add worker** with its announced name, source address, waiting since, its **Worker fingerprint** and its public pairing id (the first four characters of the code, not the secret). Its console box then says: "Enter the code on the Hub (Workers > Add worker), or admit the request in Workers > Pairing requests after checking these words".

Choose **Admit** only when the Worker's console shows the same Worker fingerprint and the same Hub fingerprint as the row (the dialog shows both; the row shows the Hub key that answered that Worker). The Hub then enrolls exactly the key the request presents (the Worker proves it with its signature), the Worker pins the Hub key it showed, and the Worker joins the admitting administrator's space **approved** (the admission is the approval; move it to its owner afterwards if needed). **Reject** removes the request: the Worker shows "The pairing request was rejected on the Hub" and a new code, and requests of the same Worker key stay hidden from the list for 10 minutes (its code still works).

- Only administrators see and admit requests (operations `pairing_requests_list`, `pairing_request_admit`, `pairing_request_reject`, also through the API and MCP with an administrator's key that holds manage). A user without that right types the code: it proves that they see the Worker's screen, and an admitted Worker's owner gets its terminal sessions.
- A Worker offers a request only when its Hub key is checked by more than a person's comparison: `hub_fingerprint` is set, or the Hub's TLS certificate is trusted by the Worker's computer for the Hub's host name on the very connection that carried the upgrade (a public certificate on your reverse proxy; no redirect, no HTTP proxy in the path). Otherwise only its code pairs, because nobody may be looking at the words, and the Worker does not appear in the list.
- Requests are the waiting connections themselves: at most 16 per source address and 1024 in total, and they end with the code (10 minutes).
- Details and the guarantees: [security](../security.md#pairing-requests).

### If pairing is interrupted

Pairing is **idempotent per pairing code and Worker key**. If the connection ends after the Hub accepted the code but before the Worker recorded the result (a lost answer, a crash of the Worker), the Worker keeps the code and the Hub identity it authenticated, and on the next start says `Resuming an interrupted pairing with the same code.` above the same code. Enter the same code again, as the same user who entered it the first time, before it expires (10 minutes). The Hub returns the Worker it already created (same id, name and approval state; audit `worker.pair_resume`) instead of failing with `already_paired`. Only a Worker that holds the same key can resume, a wrong code counts against five attempts, and a Worker that finds a different Hub identity answering refuses it.

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

The key carries the Hub's identity fingerprint, so a Worker that enrolls with it pins the Hub the key names without trusting any TLS certificate. That protects only when the key reaches the Worker without passing through a page a proxy can rewrite: create it on the Hub host ([below](#when-you-do-not-want-to-rely-on-the-proxy)). If the Worker also has `hub_fingerprint`, both must name the same Hub, or it stops before anything is sent.

A key does not make a Worker usable by itself: a Worker that enrolls with it is **pending** until you approve it, so a stolen or leaked key cannot operate anything.

### Approve a new Worker

Every new Worker starts **pending**, whichever way it joined: pairing code, invitation key or the Hub-wide infrastructure key. A pending Worker may connect, and the Hub shows it as such, but the Hub routes no operation to it (it cannot be a target of any user, key or other Worker), and the calls it makes to the Hub are refused with `worker_pending`. The one thing it may still do is retire itself (`minion worker reset` on its machine, or removing it in the Hub).

The owner of the Worker, or an administrator, approves it:

- **In the Hub:** the **Workers** page shows a *pending approval* badge and a banner. Each pending Worker shows its **fingerprint** (six words and the 52 characters of its key; the Worker console shows the same in its box: compare all six words), the **host name** it announced and the **source address** it enrolled from; compare them with the machine you just started, then choose **Approve**, or **Remove** if you do not recognise it. The Worker reconnects within moments and then receives operations.
- **From the command line:** `minion hub worker approve <fingerprint|id>` (the fingerprint as 52 characters or six words; words match pending Workers only) with `MINION_HUB_URL` and `MINION_API_KEY_FILE` set. The argument is a Worker **id** or a **fingerprint**, and the two need different permissions. An id is approved directly and needs only *manage* (or *operate*) on that Worker, so an administrator's API key with *manage* alone can approve exactly the Worker that automation started, if it knows its id. A fingerprint is first looked up in the Workers list, so that form also needs *read* on the Worker. `minion workers_list --all` shows ids and fingerprints.
- **Through the API:** the operation `worker_approve` (`worker_id`, and optionally the `fingerprint` you looked at; the Hub approves only if it matches). It needs *operate* or *manage* on the Worker; a Worker identity, and an API key bound to that very Worker, can never approve it.

The Worker's log says `this Worker is waiting for approval` while it waits. Both the enrollment and the approval are in the audit log (`worker.enroll` with outcome `pending`, then `worker.approve`).

Workers that existed when you upgraded to 0.6 were approved by the upgrade. Invitation keys of non-admin users that had no expiry now expire 7 days after the upgrade, and reusable keys without a limit allow 10 more enrollments; the Invitations page shows a notice on each key that was limited.

### The infrastructure key file

`MINION_INFRA_INVITE_KEY_FILE` names a file with one Hub-wide key that enrolls infrastructure Workers (owned by `infra_default_owner`, or the first administrator). The Hub keeps it as a row of the invitation list, keyed by the hash of its secret, with an **expiry** (`infra_invite_key_days`, default 30 days from the first load; `0` means none and the Hub warns at every start). The setting applies when the Hub first records a key: changing it later does **not** change the expiry of an existing key. To change the lifetime, set `infra_invite_key_days` **and** rotate the secret in the key file (or revoke the existing key), a **use counter** and a **revocation** that sticks. Restarting the Hub with the same file never resets the expiry or the counter and never undoes a revocation. Administrators see the row in **Invitations** and revoke it there (or with `invitation_revoke`) like any key; to rotate, put a new key in the file. Workers it enrolls are pending like all others. The key is bound to the Hub identity, so a Hub that has no root key yet (a new Hub in the state B0, before the passphrase is set) **refuses to start** while `MINION_INFRA_INVITE_KEY_FILE` is set, and its message tells you to unset it, start the Hub, set the Hub passphrase and set the variable again ([Hub identity](hub-identity.md)). A Hub migrated from 0.6 already has its root and starts with the file.

## When you do not want to rely on the proxy

Everything a browser shows passes through the proxy in front of the Hub. These steps, run on the Hub host, keep it out of pairing.

- **Read the fingerprint:** `minion hub identity --words` (Docker: `docker exec <hub container> minion hub identity --words`).
- **Enter a pairing code on the Hub host**, so that no proxy sees it. The key file is an API key of the user who will own the Worker:

  ```sh
  MINION_API_KEY_FILE=<key file> minion pairing_claim --code MN-XXXX-XXXX \
    --hub-url https://127.0.0.1:8443 --hub-ca-file <data dir>/tls/cert.pem
  ```

  With `MINION_TLS=off` use `--hub-url http://127.0.0.1:8443` and no CA file.
- **Create an invitation key there** (`minion invitation_create --mode one_time --ttl 1h`, with the same Hub flags) and copy it to the Worker yourself. The fingerprint inside the key is then authentic, which makes a Docker Worker safe without a prompt.
- **Approve there:** `minion hub worker approve <the six words from the Worker console>`, with the same Hub flags.

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

- **Workers** in the Hub lists the Worker with its owner, kind and whether it is online, or pending approval. `minion worker status` shows the same from the Worker's side: the connection state, the Hub fingerprint (words and full), the Hub check, the TLS path, the Worker fingerprint and the share choice ([troubleshooting](troubleshooting.md#worker-status-as-json)).
- To end a pairing, remove the Worker in the Hub: it becomes a tombstone, its trust and bound API keys are deleted, and the Worker is told to wipe its pairing. From the machine's side, `minion worker reset` (the Hub is not told: remove the Worker in the Hub as well).
- Hub administrators can rename, reassign and restrict Workers (to preview only, which refuses every write to a session while it is on).
