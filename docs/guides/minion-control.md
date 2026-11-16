# Minion control: master sessions, slave sessions and who accepts whom

Back to the [README](../../README.md).

Minion control decides which sessions may control sessions on **other** Workers. It replaces Administration > Session control (0.8) and keeps its grants (see [Upgrade](#upgrade-from-session-control)).

## The model

- **User-managed** (default on for every Worker): people and API keys control the Worker's sessions through the web UI, the CLI and MCP, as always, within their own permissions. Turn it off and people and API keys can no longer read or type into that Worker's sessions; only master sessions of accepted Workers can. Its owner and administrators still manage the Worker (sharing, updates, removal).
- **Master session**: a session nominated on its Worker as one that may control sessions on other Workers. **None by default.**
- **Slave session**: every other session, the default. It controls nothing beyond its own Worker.
- **Accepts control from**: the Workers (or **All Workers**) whose master sessions a Worker accepts. **Nobody by default.** A Worker that accepts at least one Worker is *minion-managed*.

A master session **M** on Worker **A** controls session **T** on Worker **B** only when all of these hold (the **chain of trust**, checked by the Hub on every call):

```text
1. M is a master session on A          (nominated)
2. B accepts control from A, or all    (B's consent)
3. B shares T                          (B's sharing rule)
4. A's owner may reach B               (an administrator, or the same owner)
5. B is not preview-only               (for writes)
```

Control is one-directional: B's master sessions do not reach A unless A accepts B.

## The screen: Access > Minion control (administrators)

- **Workers**: one row per Worker. **User-managed** is a checkbox. **Accepts control from** has **All Workers** and a filterable list of the other Workers. **Master sessions** lists the nominations; **Add a master session** offers the session names the Hub sees on that Worker and a field for any name. Thin Workers and Workers older than 0.8.0 cannot be masters.
- **Matrix**: rows are the Workers that have master sessions, columns all Workers; a tick means "the column accepts control from the row". Use it for an overview of many Workers; the filter at the top narrows rows.
- **Who can control this session?**: pick a Worker and a session name to see the master sessions that reach it, or why none does.
- Every change is recorded in the audit log with the changed Worker; a change that was decided before the Worker was removed or given to another owner is refused.

Every click is saved at once and answered with the row as the Hub now has it, with **Undo**. If the answer is lost, the page reloads what was saved instead of guessing.

The same is available as administrator operations: `control_list`, `control_set_worker`, `control_set_master`, `control_set_accept`, `control_check` (REST, `minion`, MCP).

## Use it from a master session

Exactly as before ([Session control](session-control.md#use-it-from-the-controller-session)): inside the master session, without `MINION_HUB_URL`, `minion workers_list`, `minion sessions_list --hub-worker <id>`, and `--target <worker-id>:<session>` on the session commands, or `minion mcp --allow-writes`.

## What it does not check

The session name is taken from the terminal the request comes from: anyone who can run programs as the Worker's user on that host can claim to be the master session. Treat a master session as trust in its whole **host and account**, and keep untrusted work on other Workers or accounts. The other limits of [session control](session-control.md#what-a-grant-does-not-check) apply too.

## Upgrade from session control

The Hub migrates every 0.8 grant: its controller session becomes a master session that keeps its exact old targets as a limit (shown as **limited**), and every target Worker accepts control from the controller's Worker. Nothing reaches more or less than before. Ticking the master session again in the new screen removes the old limit. `session_control_set` with targets is refused with a pointer to this page (it could widen other master sessions of the same Worker); without targets it still ends a master session. `session_control_list` shows what master sessions reach now.
