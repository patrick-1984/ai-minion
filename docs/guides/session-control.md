# Session control: let one session drive sessions on other Workers

Back to the [README](../../README.md).

A **session-control grant** lets one tmux session on one Worker, the **controller session**, list, read and type into sessions on other Workers with the local `minion` command line and `minion mcp`. It is meant for an agent that coordinates other agents: the agent in `build-box:boss` can read the screen of `ci-runner:x` and type into `laptop:build` without an API key, a call key or any setting on the target Workers. Every other session behaves as before: it reaches its own Worker through the local control socket and nothing on other Workers.

## Default: nobody has a grant

No Worker and no session holds a grant until an administrator creates one. There are no grants after installing or upgrading the Hub, none for a newly paired Worker or a new session, and none that follow from owning a Worker or from the administrator role. Without a grant, every cross-Worker call from a session is refused.

## Create a grant in the web UI

Administrators only: open **Administration → Session control**.

1. Under **Controller Worker** choose the Worker that runs the controller session, and under **Controller session** type the name of that session (for example `boss`).
2. Under **Targets** choose:
   - **All**: every Worker and session that the owner of the controller Worker may reach. For an administrator's Worker that is the whole fleet.
   - **Selected**: only what you tick. Tick a Worker to grant all of its sessions, or tick some of its sessions only. The form lists the sessions of the Workers that are online and answer within 2 seconds.
3. **Save the grant**. Saving again for the same controller session replaces its targets.

The form opens with nothing selected, and saving with no targets creates nothing. The **Grants** table lists every grant (controller session and its targets); **Remove** ends it. A removed grant, a removed Worker or a Worker given to another owner stops working on the next call: the Hub reads the grants for every request.

The same is available as the admin operations `session_control_list` and `session_control_set` (REST and `minion` against a Hub with an administrator key); `session_control_set` with no targets removes the grant.

A grant gives `read`, `operate` and `message` on its targets, and never more than the owner of the controller Worker has: if the owner loses access to a Worker, the controller loses it too. The Worker's own rules still apply on the target: an unshared session and a preview-only Worker refuse as they do for anyone. The target's `pipe_accept_from` is not consulted for a request that the grant authorized.

## Use it from the controller session

Run the commands inside the controller session on the controller Worker, **without** `MINION_HUB_URL`. The local command line sends the session's `$TMUX_PANE` to the local Worker, which names the session to the Hub and forwards the request on its own connection. A target on the same Worker stays on the local socket as before.

```sh
minion workers_list                                   # the Workers this session may reach, with their ids
minion sessions_list --hub-worker <worker-id>         # the sessions of one of them
minion session_snapshot --target <worker-id>:x        # read a screen
minion send_text --target <worker-id>:build --text 'make test' --submit
```

- Name other Workers by their **id** (a UUIDv7) in `--target` and `--hub-worker`; without a Hub URL a Worker name is refused. `workers_list` prints the ids. It lists the controller's own Worker and the Workers the grant reaches as a whole; a Worker of which only some sessions are granted is not listed. Its id is in the address of its page in the web UI (click its name in **Workers**: `/workers/<worker-id>`), or an administrator lists every Worker with `minion workers_list --all` and a Hub key ([API keys and MCP](api-keys-and-mcp.md#use-a-key)).
- The forwarded operations are `workers_list`, `sessions_list`, `session_snapshot`, `session_capture`, `send_text`, `send_keys`, `set_task` and `answer`. Creating sessions and changing sharing on other Workers are not part of a grant.
- A target outside the grant answers `not_found` (the Hub does not say whether it exists); a session without a grant gets `forbidden`.
- A write whose connection to the Hub ends before the answer is reported with the outcome `unknown` and the warning `hub_call_lost`: it may have been applied. Repeat it on purpose with the same `--request-id`; the target never applies the same request id twice within 10 minutes.
- With `MINION_HUB_URL` set, the commands go to the Hub with your API key as before, and the grant plays no part.

For an agent, register `minion mcp` in the controller session. It offers the Worker operations as tools; the writes (`send_text`, `send_keys`, `set_task`, `answer`) only with `--allow-writes`:

```sh
claude mcp add minion-control -- minion mcp --allow-writes
```

The `workers_list` tool gives the agent the Worker ids. Pass the Worker id in the tool's `target` (`worker_id` and `session_name`); for `sessions_list` start the server with `--hub-worker <worker-id>` (or `MINION_HUB_WORKER`).

## Minimum version

The Hub, the controller Worker and every target Worker must run **0.8.0 or later**. A target Worker older than 0.8.0 does not know the grant: it still answers reads, and it treats a write from a controller like a message from another Worker, under its own `pipe_accept_from`. With the default `none` it refuses the write with `pipe_origin_refused`; with `owner-workers` or a list that names the controller Worker it accepts it. A controller Worker older than 0.8.0 refuses a request that names another Worker before anything is sent, and the client says that the Worker must be updated.

## What a grant does not check

These are deliberate simplifications; plan for them when you create a grant.

- **The session name is taken on trust.** The controller Worker reports which session a request comes from, by the pane of the calling process, and nothing verifies it further. Any process on the controller's host that runs as the Worker's user can claim the controller session. Treat a grant as a grant to the whole **host** of the controller Worker; with **All** that host reaches every Worker the owner may reach.
- **The grant follows the name.** A session created again with the same name has the grant; a renamed session loses it.
- **Sessions on the same Worker are not restricted.** Any session on a Worker can already reach the other sessions of that Worker through the local socket, with or without a grant.
- **Call limits.** Forwarded requests use the controller Worker's call streams to the Hub, which the pipe uses too, and the Hub takes only a few of them at once from one Worker (8 in flight, 16 open streams). Heavy polling can therefore be answered `rate_limited`; poll slowly and retry later.

See also [security](../security.md#worker-isolation-one-compromised-worker-does-not-reach-the-others) and, for agent-to-agent messages that the receiver must accept, [the pipe](pipe.md).
