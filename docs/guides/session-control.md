# Session control: let one session drive sessions on other Workers

Back to the [README](../../README.md).

**Start with [Access: who reaches what](access.md).** It describes the current model: direct grants from one session to another (View, Message, Control), master sessions, who may decide and how to find out why a call is refused. This page is kept for the calls a session with access makes, which are unchanged, and for the two operations of 0.8.0.

A session reaches sessions on other Workers in two ways: with a **grant** made on **Access > Grants** ([Access](access.md)), or as a **master session** nominated on **Access > Minion control** ([Minion control](minion-control.md)). In both cases it lists, reads and types with the local `minion` command line and `minion mcp`, without an API key, a call key or any setting on the target Workers: the agent in `build-box:boss` can read the screen of `ci-runner:x` and type into `laptop:build`. This page calls that session the **controller session**. Every other session behaves as before: it reaches its own Worker through the local control socket and nothing on other Workers.

## Default: nobody has access

No session reaches another Worker until someone decides it: the owner of both Workers or an administrator makes a grant, or an administrator nominates a master session and lets Workers accept control from its Worker. There is no such access after installing or upgrading the Hub, none for a newly paired Worker or a new session, and none that follows from owning a Worker or from the administrator role. Without it, every cross-Worker call from a session is refused.

## The 0.8.0 grants and their operations

The 0.8.0 form (**Administration > Session control**: one controller session with a list of targets, or **All**) can no longer be created. From 0.9.0 the Hub computes that reach from master sessions and acceptances ([Minion control](minion-control.md#upgrade-from-session-control)), and new access for one session is a grant ([Access](access.md)).

- `session_control_list` (administrators; REST and `minion` against a Hub) shows what master sessions reach now.
- `session_control_set` with targets is refused with `conflict` and a pointer to Minion control. Without targets it still ends a name-only master session; for a session-bound master it is refused, and the master is revoked on **Access > Minion control**.
- The old page `/admin/session-control` has no navigation entry. Its table and **Remove** still work; its form to create a grant always answers with that refusal.

Access through a master session gives `read`, `operate` and `message`, and never more than the owner of the controller Worker has. Access through a grant gives what its level says ([levels](access.md#levels)). The Worker's own rules still apply on the target: an unshared session and a preview-only Worker refuse as they do for anyone. The target's `pipe_accept_from` is not consulted for a request that the Hub authorized this way.

## Use it from the controller session

Run the commands inside the controller session on the controller Worker, **without** `MINION_HUB_URL`. The local command line sends the session's `$TMUX_PANE` to the local Worker, which names the session to the Hub and forwards the request on its own connection. A target on the same Worker stays on the local socket as before.

```sh
minion workers_list                                   # the Workers this session may reach, with their ids
minion sessions_list --hub-worker <worker-id>         # the sessions of one of them
minion session_snapshot --target <worker-id>:x        # read a screen
minion send_text --target <worker-id>:build --text 'make test' --submit
```

- Name other Workers by their **id** (a UUIDv7) in `--target` and `--hub-worker`; without a Hub URL a Worker name is refused. `workers_list` prints the ids. It lists the controller's own Worker and the Workers the session reaches. Its id is in the address of its page in the web UI (click its name in **Workers**: `/workers/<worker-id>`), or an administrator lists every Worker with `minion workers_list --all` and a Hub key ([API keys and MCP](api-keys-and-mcp.md#use-a-key)).
- The forwarded operations are `workers_list`, `sessions_list`, `session_snapshot`, `session_capture`, `send_text`, `send_keys`, `set_task` and `answer`. A grant below Control forwards fewer of them ([levels](access.md#levels)). Creating sessions and changing sharing on other Workers are never included.
- A target the session does not reach answers `not_found` (the Hub does not say whether it exists); a session without any access gets `forbidden`. **Access > Why?** shows the reason ([Access](access.md#find-out-why-a-call-is-refused)).
- A write whose connection to the Hub ends before the answer is reported with the outcome `unknown` and the warning `hub_call_lost`: it may have been applied. Repeat it on purpose with the same `--request-id`; the target never applies the same request id twice within 10 minutes.
- With `MINION_HUB_URL` set, the commands go to the Hub with your API key as before, and the session's access plays no part.

For an agent, register `minion mcp` in the controller session. It offers the Worker operations as tools; the writes (`send_text`, `send_keys`, `set_task`, `answer`) only with `--allow-writes`:

```sh
claude mcp add minion-control -- minion mcp --allow-writes
```

The `workers_list` tool gives the agent the Worker ids. Pass the Worker id in the tool's `target` (`worker_id` and `session_name`); for `sessions_list` start the server with `--hub-worker <worker-id>` (or `MINION_HUB_WORKER`).

## Minimum version

The Hub, the controller Worker and every target Worker must run **0.8.0 or later**; a grant at level Message also needs a target Worker that announces the agent-only `send_text`. A target Worker older than 0.8.0 does not know the grant: it still answers reads, and it treats a write from a controller like a message from another Worker, under its own `pipe_accept_from`. With the default `none` it refuses the write with `pipe_origin_refused`; with `owner-workers` or a list that names the controller Worker it accepts it. A controller Worker older than 0.8.0 refuses a request that names another Worker before anything is sent, and the client says that the Worker must be updated.

## What a grant does not check

These are deliberate simplifications; plan for them when you give a session access. They hold for name-only master sessions and for grants ([Access](access.md#identity-honestly)).

- **The session name is taken on trust.** The controller Worker reports which session a request comes from, by the pane of the calling process, and nothing verifies it further. Any process on the controller's host that runs as the Worker's user can claim the controller session. Treat access for a session as access for the whole **host and account** of the controller Worker. (A session-bound master in a prepared Claude Code terminal is the exception: the Hub verifies that terminal.)
- **Access follows the name.** The Hub deletes a session's grants and its master nomination when it sees the session end or stop being shared, or when its Worker no longer lists it. A session that is replaced under the same name before the Hub notices keeps them.
- **Sessions on the same Worker are not restricted.** Any session on a Worker can already reach the other sessions of that Worker through the local socket, with or without a grant.
- **Call limits.** Forwarded requests use the controller Worker's call streams to the Hub, which the pipe uses too, and the Hub takes only a few of them at once from one Worker (8 in flight, 16 open streams). Heavy polling can therefore be answered `rate_limited`; poll slowly and retry later.

See also [security](../security.md#worker-isolation-one-compromised-worker-does-not-reach-the-others) and, for agent-to-agent messages that the receiver must accept, [the pipe](pipe.md).
