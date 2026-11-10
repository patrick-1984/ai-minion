# API keys, the command line against a Hub, and MCP

Back to the [README](../../README.md).

Everything the web UI does is an **operation** of the Hub's REST API, and the `minion` command line and the MCP server call the same operations. A caller proves who it is with an **API key** that is limited to chosen users, Workers or sessions and to chosen actions.

## Create an API key

In the web UI open **API keys**. From the command line, once you have a first key (the first one is made in the UI):

```sh
minion apikey_create --name ci-reader --actions read \
  --resources '[{"kind":"worker","worker_id":"<worker-id>"}]' \
  --expires-at 2027-01-01T00:00:00Z
```

- **Resources** are what the key may touch: `{"kind":"worker","worker_id":"..."}`, `{"kind":"session","worker_id":"...","session_name":"..."}`, `{"kind":"user","user_id":"..."}` or `{"kind":"everything"}` (not combined with others). A user can grant only resources they own; `everything` is for administrators.
- **Actions**: `read` (lists, snapshots, captures, pipe turns), `operate` (type, keys, tasks, create sessions, and `share_set`, which changes what the Hub may see), `message` (send messages to agent sessions through the pipe, nothing else) and `manage` (administration; administrators only). A key holds the chosen actions on every listed resource.
- **Expiry** is optional and worth setting. **Bound Worker**: `--bound-worker-id` binds a key to one Worker's identity; it is accepted only on that Worker's own call streams and refused on REST.
- The key (`mn_...`) is shown **once**. Only a hint (its first characters) is kept; `apikey_list` shows hints, scope and expiry, never the key. Revoke a key at once with `apikey_revoke`, or on the API keys page.

Give a program the least it needs: a monitoring script needs `read`; an agent that only has to talk to other agents needs `message`; typing into sessions or changing sharing needs `operate`.

## Use a key

Put the key in a file that holds nothing else, readable only by you:

```sh
printf '%s' 'mn_...' > ~/.config/minion-api-key
chmod 0600 ~/.config/minion-api-key
```

The key is read **only from that file**: there is no flag and no plain environment variable for it. The file must be a regular file owned by you that group and others cannot access, and not a symbolic link. Then:

```sh
export MINION_HUB_URL=https://hub.example.com
export MINION_API_KEY_FILE=$HOME/.config/minion-api-key

minion workers_list
minion sessions_list --hub-worker <worker-id>
minion session_snapshot --target <worker-id>:<session>
```

- With `MINION_HUB_URL` and `MINION_API_KEY_FILE` set (or `--hub-url`), every command goes to that Hub and **never to the local Worker**; a failure to reach the Hub is an error, never a silent fallback to the local Worker or another Hub. The client follows no redirect (the key goes to the configured address and nowhere else), retries nothing and never sends a write twice. The Hub's certificate is verified against the system roots unless you give a private CA.
- A Hub with a private CA: `--hub-ca-file` (or `MINION_HUB_CA`). `--hub-insecure` skips the certificate check for this client only and warns.
- Operations whose arguments name no Worker (`sessions_list`, `session_create`) need `--hub-worker <id>` (or `MINION_HUB_WORKER`): the **id** (a UUIDv7, as `workers_list` prints it; a name is refused).
- A write (`send_text`, `send_keys`, `pipe_send`, ...) has a request id, by default a new one, and a repeat of the same id within 10 minutes is answered from the Worker's cache and never delivers twice. If the connection breaks after the request may have gone out the result is **unknown** (exit status 4): do not repeat it blindly; repeat it with the same `--request-id`, or look at the session first.
- Without a Hub (`MINION_HUB_URL` unset) the same commands act on the local Worker through its local control socket. The Hub-only operations, the invitation, key and user commands among them, need a Hub.

The same operations over plain HTTP. Do not put the key on the `curl` command line (`-H "Authorization: Bearer $(cat ...)"` expands it into the argument list, which every local user can read in the process list). Give `curl` the header on standard input instead, with `--header @-`; the key goes through a pipe and is never an argument of any process:

```sh
IFS= read -r api_key < "$MINION_API_KEY_FILE"
printf 'Authorization: Bearer %s\n' "$api_key" |
  curl -sS --header @- \
    https://hub.example.com/api/v1/ops/workers_list \
    -H 'Content-Type: application/json' -d '{}'
unset api_key
```

`read` and `printf` are shell built-ins, so no process gets the key as an argument (an exported variable would be visible in the environment of child processes, so `api_key` is not exported; `unset` it afterwards). Use the key file as it is; do not make a second copy of the key in a header file. If you prefer a header file for `curl`, create it with mode 0600 in a private directory (`umask 077`), pass it as `--header @<file>` and delete it afterwards. It is a credential as much as the key file is. Reading the key through `minion` itself, as above, needs none of this.

Operations that only read also answer `GET` with the arguments in the query string; operations that change state are `POST` only.

## MCP: Minion as tools for an agent

`minion mcp` serves the operations as MCP tools on standard input and output, for Claude Code, Codex and other harnesses. Register it as a stdio server whose command is `minion mcp`, for example:

```sh
claude mcp add minion -- minion mcp
claude mcp add minion-hub \
  --env MINION_HUB_URL=https://hub.example.com \
  --env MINION_API_KEY_FILE=$HOME/.config/minion-api-key \
  -- minion mcp
```

```toml
# Codex: ~/.codex/config.toml
[mcp_servers.minion]
command = "minion"
args = ["mcp"]
```

Without a Hub the tools run on the **local Worker** through its control socket, so a Worker must be running for this user (`minion worker run`); with `MINION_HUB_URL` and `MINION_API_KEY_FILE` set they call that Hub with that key instead, and the Hub decides which sessions the key may use.

### The default is read-only

The MCP server exposes only tools that **do not change anything**: listing sessions, snapshots, captures, reading pipe turns and `share_get`. The tools that change state (`session_create`, `send_text`, `send_keys`, `set_task`, `answer`, `pipe_send` and `share_set`) are not offered, not even listed as read-only, and a call to one is refused, unless you start the server with `--allow-writes` (or `MINION_MCP_ALLOW_WRITES=true`):

```sh
claude mcp add minion-rw -- minion mcp --allow-writes
```

Think before you do. A tool that can type into a session can make that session's agent run commands, and a model that can type into sessions can be talked into doing it by anything it reads. Terminal screens are sanitised on the Worker before they reach the model, but their meaning is still the session's. Prefer a Hub key with only the actions and the Workers the model needs, over writes on a Worker that serves all your sessions. See [security](../security.md).

### Sharing through MCP

`share_get` and `share_set` name the Worker that runs them in `worker_id`, which is required. Through a Hub (the normal case) use the id that `workers_list` prints. Against a local Worker the id has to be that Worker's own id; **an id of another Worker is refused** and changes nothing, and nothing is ever redirected to the local Worker. A model with `--allow-writes` can change what the Hub may see of a Worker, including making all of its sessions visible, so grant `share_set` as carefully as typing. A Worker can forbid it for everyone: start it with `--share-lock` ([pairing and sharing](pairing.md#choosing-what-the-hub-sees)).

### Pipe replies and trust

`pipe_read` and `pipe_wait` do not give a model the replies of a source its user has not trusted; see [the pipe](pipe.md).
