# Access: who reaches what

Back to the [README](../../README.md).

This page explains how a session on one Worker gets to read or type into a session on **another** Worker, who decides that, and how to find out why a call is refused. It covers the Hub pages under **Access** and the operations behind them.

One default holds everywhere: **nothing crosses Workers until someone decides it.** A new Hub, a new Worker and a new session have no access to other Workers.

## The model in one page

A **grant** is one decision: *source session S on Worker A may reach target T on Worker B at a level.*

```text
 grant:  session wb-4 on patryk-work  --may reach-->  session wb-3 on patryk-f5   at level Message
         (source: always one named session)           (target: one session, or every shared session of the Worker)
```

- The source is always one named session. The target is one session, or **every shared session** of the target Worker, future ones included.
- Source and target are on different Workers. A grant inside one Worker is refused: every session of a Worker can already reach the others on that host through the local socket, so a grant there would promise a boundary that does not exist.
- A grant only allows. There are no deny rows: to take access away, revoke the grant.

### Levels

Each level includes the ones below it.

| Level | What the source session can do on the target |
| --- | --- |
| **View** | `workers_list` and `sessions_list` (only what it reaches), `session_snapshot`, `session_capture`: see the screen. |
| **Message** | View, and `send_text` as a **submitted prompt to an AI agent pane**. The Hub turns the call into an agent-only send, requires `submit` and refuses `force`. The agent decides what to do with the prompt. |
| **Control** | Message, and `send_text` into any pane, `send_keys`, `set_task`, `answer`. |

Never part of a grant: creating sessions on the target (`session_create`), changing its sharing (`share_set`), managing the Worker. `force` never overrides a missing permission.

Message needs a target Worker that announces the agent-only send; an older Worker answers `unsupported_op` and nothing is sent. Sending over [the pipe](pipe.md) with a Message grant is planned with a later Worker release and does not work yet.

### Master sessions

A **master session** is the broad form that existed before grants and still works unchanged: a session that an administrator nominated on its Worker reaches every shared session of every Worker that **accepts control from** its Worker, always at level Control. It is set on **Access > Minion control** ([Minion control](minion-control.md)).

Grants and master reach add up. A session can be a master and hold grants; the matrix shows both and names where each cell comes from ("via master" or a grant). Use a grant when one session needs one target or less than Control; use a master session for a coordinator that should reach whole Workers.

### The layers: all of them must allow a call

```text
 1 Caller      who is calling: a Verified terminal, or a session name Claimed by its Worker
 2 Grant       a grant or master reach covers source -> target at the level the call needs
 3 Authority   the decision still stands (see "Who may decide")
 4 Time        the grant is not expired and not suspended
 ------------  decided by the Hub on every call; running calls are checked again within 30 seconds
 5 Sharing     the target Worker shares the session; preview-only refuses writes   (the target Worker decides)
 6 Workers     both Workers are approved and connected, the target session exists
```

Layers 1 to 4 are the Hub's decision. Layer 5 belongs to the target Worker and its owner: the Hub cannot override a share rule or preview-only. Nothing is cached: a revoked, changed, expired or suspended grant stops working on the next call, and a call that is still running (a stream) is ended within 30 seconds.

## Who may decide

| Action | Owner of both Workers | Administrator | API key | A session or a Worker |
| --- | --- | --- | --- | --- |
| Create a grant, or change its level, expiry or note, between Workers of one owner | yes | yes | no | no |
| The same between Workers of different owners | no | yes | no | no |
| Revoke | yes, for a grant that touches one of their Workers | yes | yes, with `operate` on one of the two Workers | no |
| Approve a suspended grant again | no | yes | no | no |
| Nominate a master session, set "accepts control from" | no | yes | administrator keys | no |

- **Creating and approving need a signed-in browser session.** An API key, a Worker and a session are refused with `forbidden` ("this operation is available only to a signed-in browser session"). The reason: a grant is durable reach for a program, and a program that holds an API key must not be able to enlarge its own reach. An administrator who acts as another user is refused too.
- Each grant records its **authority**. `owner`: both Workers belonged to the same owner when it was decided, and it works only while they still do. `admin`: an administrator decided it (always the case between different owners), and it works only while that administrator is an active administrator.
- Both Workers must be approved. A thin Worker cannot be a source.
- The person who created a grant can revoke it while they own one of the Workers or are an administrator. Someone who lost the administrator role and owns neither Worker can no longer revoke it; any administrator or either owner can.

## Identity, honestly

Every source session is shown with how the Hub knows it.

| Shown as | What it means |
| --- | --- |
| **Verified** | The calling terminal is proven by the kernel and holds a short lease. Today this exists only for session-bound master sessions in a prepared Claude Code terminal on Linux ([AI agents](agents.md#prepare-one-claude-code-terminal)). |
| **Claimed** | The session is a **name reported by its Worker**, taken from the pane of the calling process. Nothing verifies it further. |

Every grant made today has a Claimed source; Verified sources for grants are planned. Plan for what Claimed means:

- **Any program that runs as the Worker's OS user on that host can use the grant.** Treat a grant from a Claimed session as a grant to the whole account of the source Worker, and keep untrusted work on other Workers or accounts.
- A session name can come back as another terminal. The Hub deletes grants when it sees the session end, be renamed or stop being shared (see [What ends a grant](#what-ends-or-suspends-a-grant)). A replacement that happens between two observations of the Hub is not noticed; an instance id per session is planned with a Worker release.

## The pages

**Access** is in the navigation of every signed-in user. An administrator sees everything. Other users see the pairs that have one of their own Workers at either end; a session of another owner appears as "a session of <owner>", not by its name.

| Page | Address | What it answers |
| --- | --- | --- |
| **Who reaches what** (the matrix) | `/access` | Which session reaches which session on another Worker **right now**, and which layer blocks a pair. Rows are source sessions, columns are target sessions, grouped by Worker. A cell shows what is in effect: `V`, `M` or `C`, `C★` for master reach, `⊘ blocked:` with the layer, `·` for none. Read-only. **Refresh** reloads it, "As of" says how old it is, **Print** gives a print layout. A cell opens **Why?** for that pair. |
| **Grants** | `/access/grants` | Every grant you may see, with source, target, granted level, state (active, expired, suspended), who decided and until when. This is where grants are added, changed, approved and revoked. Master reach is not listed here. |
| **Why?** | `/access/why` | One source and one target: every check in order (caller, grant, authority, time, sharing, Workers, preview-only), each as pass, fail, not evaluated or unknown. The first failing check is what blocks the pair. |
| **Minion control** (administrators) | `/access/control` | Master sessions, which Workers accept control from which, and the **User-managed** switch of each Worker. See [Minion control](minion-control.md). |

The matrix and **Why?** are computed by the same code. They are a snapshot: nothing decides from them. Preview-only is always "unknown" there, because the target Worker decides it and does not report it to the Hub. The selects on **Why?** offer the sessions that appear in the matrix; another pair can be asked with `access_explain`.

Planned, not in this release: an Access panel on the session and Worker pages, a drawer in the matrix, CSV export, a preview and a typed confirmation in the Add dialog, **Undo** after adding.

## Step by step

### Let one session read another

1. Open **Access**, then **Grants**, then **Add grant**.
2. **Source**: the Worker and the session that should get access. The list offers sessions that are online and shared; as an owner you choose among your own Workers.
3. **Target**: the other Worker, and one session or "every shared session of this Worker".
4. **Level**: **View**. **Until**: no end, 7 days or 30 days. A note is optional.
5. **Add grant**. The grant is listed at once and works on the next call of the source session.

The target Worker must share the target session ([pairing and sharing](pairing.md)); a grant does not change sharing.

### Let one agent prompt another

The same, with level **Message**. The source session can then read the target and send it a prompt:

```sh
minion send_text --target <worker-id>:reviewer --text 'Please review the open branch.' --submit
```

`--submit` is required with a Message grant; without it the call is refused with `invalid_argument`. The text goes to an AI agent pane only. A Message grant cannot send keys, answer a dialog or type into a shell. What the agent does with the prompt is the agent's decision: grant Message only to a session whose prompts the target may act on.

### Give Control between users (administrator)

1. Sign in as an administrator and open **Access > Grants > Add grant**.
2. Choose the source session on one user's Worker and the target on the other user's Worker.
3. Choose **Control** and under **Until** prefer 7 days or 30 days over "no end": the source is Claimed, so the grant is in fact for the whole account of the source Worker.
4. **Add grant**.

The grant has the authority `admin` and names you as the person who decided. The owner of either Worker sees it under **Grants** and can revoke it.

### Change or revoke

On **Grants**, each row has **Change level** (View, Message, Control, then **Save**) and **Revoke**. A change is made for the revision you looked at: if someone else changed the grant meanwhile, the page says so and shows the current state. A revoked grant is gone; to give the access back, add it again.

### Find out why a call is refused

1. Open **Access > Why?**, pick the source session and the target session, **Check**. Or click the cell of the pair in the matrix.
2. Read the first check that fails:

| Check | Typical failure | What to do |
| --- | --- | --- |
| Grant | "no grant and no master reach covers this pair" | Add a grant, or check the session name: the target Worker may not list a session of that name now. |
| Authority | "the administrator who decided this grant is no longer an active administrator", or a Worker changed owner | An administrator approves it again, or makes a new grant. |
| Time | suspended, or expired | Approve it (administrator), or add a new grant. |
| Sharing | "the session is not shared by its Worker" | Share the session on the target Worker. |
| Workers | a Worker is offline or waits for approval | Start or approve the Worker. |

What the calling session sees: a target it does not reach answers `not_found` (the Hub does not say whether it exists); a session with no access at all gets `forbidden`; a write beyond the level is refused with `forbidden`; heavy polling can be answered `rate_limited`.

## What ends or suspends a grant

| Event | Effect |
| --- | --- |
| The source or target session ends, is renamed or stops being shared | Grants that name that session are deleted. A wildcard target ("every shared session") stays. A session that comes back under the same name starts without grants: the Hub tells a new terminal by when it first saw the name and, from Workers 0.23.0 on tmux hosts that report an instance id, by the session's instance, also when the name was recreated within one poll, while the Worker was offline or while the Hub was down. A Worker that reports no instance id (older Workers, macOS, psmux, ConPTY) is judged by the name: a session recreated under its name while the Worker was offline, or before the first snapshot after a Hub restart, keeps the grants of the old one. |
| A Worker is removed | Every grant that touches it is deleted. |
| A Worker changes owner | Every grant that touches it is deleted, in the same transaction. |
| The administrator who decided an `admin` grant loses the role or is deactivated | The grant is **suspended** at once. It stays suspended when the role comes back. |
| The expiry passes | The grant stops working at once and is shown as expired; the Hub deletes it 30 days later. |
| Revoke | The grant is deleted. A running call ends within 30 seconds; a write that was already admitted keeps its outcome. |

**Re-approval.** A suspended grant gives no access and cannot be changed. An administrator opens **Grants**, ticks "Suspended or expired" and presses **Approve** on the row. That administrator becomes the person who decided the grant. A grant that should not come back is revoked instead.

Create, change, approve and revoke are recorded in the audit log (`grant.create`, `grant.update`, `grant.approve`, `grant_revoke`) with the grant id, the source session and the target, refusals included. Grants deleted by the system (a session that ended, a Worker that changed owner or was removed, the 30-day cleanup) are not recorded yet.

## Use a grant from the source session

Run the commands **inside the source session** on the source Worker, **without** `MINION_HUB_URL`. The local command line sends the session's `$TMUX_PANE` to the local Worker, which names the session to the Hub and forwards the request on its own connection. No API key is involved.

```sh
minion workers_list                                   # the Workers this session may reach, with their ids
minion sessions_list --hub-worker <worker-id>         # the sessions it reaches on one of them
minion session_snapshot --target <worker-id>:x        # read a screen (View)
minion send_text --target <worker-id>:build --text 'make test' --submit   # Message: an agent pane only; Control: any pane
```

- Name other Workers by their **id** (a UUIDv7) in `--target` and `--hub-worker`; without a Hub URL a Worker name is refused. `workers_list` prints the ids.
- `sessions_list` shows only the sessions the calling session may read, not everything the target Worker shares.
- A write whose connection to the Hub ends before the answer is reported with the outcome `unknown` and the warning `hub_call_lost`: it may have been applied. Repeat it on purpose with the same `--request-id`.
- With `MINION_HUB_URL` set, the commands go to the Hub with your API key, and grants play no part.
- For an agent, register `minion mcp` in the source session (`claude mcp add minion-control -- minion mcp --allow-writes`); the details are in [Session control](session-control.md#use-it-from-the-controller-session).

## The operations

All six are Hub operations: `POST https://<hub>/api/v1/ops/<name>` with JSON arguments; the three that only read also answer `GET`. The examples use this helper, which keeps the API key out of every argument list ([API keys and MCP](api-keys-and-mcp.md#use-a-key)):

```sh
hubop() {   # hubop <operation> '<json arguments>'
  IFS= read -r api_key < "$MINION_API_KEY_FILE"
  printf 'Authorization: Bearer %s\n' "$api_key" |
    curl -sS --header @- "https://hub.example.com/api/v1/ops/$1" \
      -H 'Content-Type: application/json' -d "$2"
  unset api_key
}
```

| Operation | Who | Example |
| --- | --- | --- |
| `grant_list` | An owner: the grants that touch their Workers. An administrator: all. `worker_id` narrows the list. | `hubop grant_list '{}'` |
| `grant_set` | Browser session only. Creates a grant, or with `id` and `if_revision` changes level, expiry and note. | body: `{"request_id":"<new UUIDv7>","source_worker_id":"<id>","source_session":"wb-4","target_worker_id":"<id>","target_session":"wb-3","level":"message","expires_at":"2026-12-31T00:00:00.000Z"}` |
| `grant_revoke` | Owner of a Worker it touches, administrator, API key with `operate` on one of the Workers. `if_revision` is optional. | `hubop grant_revoke '{"request_id":"<new UUIDv7>","id":"<grant-id>"}'` |
| `grant_approve` | Administrator, browser session only. Lifts a suspension. | body: `{"request_id":"<new UUIDv7>","id":"<grant-id>","if_revision":4}` |
| `access_matrix` | Every signed-in user and Hub-wide keys: what they may see. `worker_id` keeps the pairs with that Worker at either end. | `hubop access_matrix '{}'` |
| `access_explain` | The same; the caller must own one end or be an administrator, otherwise `not_found`. | `hubop access_explain '{"source_worker_id":"<id>","source_session":"wb-4","target_worker_id":"<id>","target_session":"wb-3"}'` |

- `grant_set` and `grant_approve` answer an API key with `forbidden`: use the **Grants** page, which sends exactly these bodies from your browser session. They have no `minion` command. Leave `target_session` out for "every shared session"; leave `expires_at` out for no end. The source and the target of an existing grant cannot be changed: revoke it and add another.
- Levels are `view`, `message` and `control`. `expires_at` is UTC with milliseconds, as shown.
- A change with a stale `if_revision`, and a second grant for the same source and target, answer `conflict`.
- The other four are also `minion` commands against a Hub, for example `minion grant_list` and `minion grant_revoke --id <grant-id>`.
- `access_matrix` returns `as_of`, `sources`, `targets` and `cells`; a cell has `granted`, `in_effect`, `via` (`master` or a grant id) and `blocked_by` (`suspended`, `expired`, `authority`, `owner_rule`, `worker_pending`, `not_shared`, `worker_offline`). `access_explain` returns `in_effect`, `via` and the `checks` with `id`, `state` (`pass`, `fail`, `skip`, `unknown`) and `text`.

The older operations stay: `control_list`, `control_set_worker`, `control_set_master`, `control_set_accept`, `control_check` ([Minion control](minion-control.md)) and `session_control_list`, `session_control_set` ([Session control](session-control.md)).

## Limits and what is not protected

- **A Claimed source is a name.** See [Identity, honestly](#identity-honestly): the unit of trust is the OS account of the source Worker.
- **Sessions on the same Worker are not separated.** No grant is needed and none can restrict them.
- **The Hub and its administrators are trusted.** A compromised Hub or administrator account can decide any access.
- **A prompt is not a command, and not harmless either.** With Message the receiving agent may act on a prompt with its own tools and permissions.
- **The matrix can be mixed for a moment.** It is read in several steps, not one transaction, and it does not know pending share changes or preview-only. A call is always decided fresh, whatever the matrix showed.
- **An `owner` grant whose owner was deactivated** is not marked as blocked in the matrix and in **Why?** yet.
- **Master reach keeps its older rules** (it depends on the role of the owner of the calling Worker and is not suspended when that changes). Converting master sessions into grants is planned.
- **The target Worker does not learn which session called.** The Hub's audit names the calling session and the grant for writes; the Worker's own records get this with a later Worker release.
- **Call limits.** Forwarded requests share the source Worker's call streams with the pipe (8 in flight, 16 open streams per Worker).

See also [security](../security.md#worker-isolation-one-compromised-worker-does-not-reach-the-others).
