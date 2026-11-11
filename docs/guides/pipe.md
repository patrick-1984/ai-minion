# The pipe: send a message to an agent and read its reply

Back to the [README](../../README.md).

The pipe is how you, a script or another agent talk to an AI agent that runs in a tmux session. `pipe_send` pastes a message into the agent's input and presses Enter; the Worker watches the session and, when the agent has answered, records the answer as a **turn**; `pipe_read` and `pipe_wait` read the turns.

## Send, read, wait

Through a Hub (see [API keys and MCP](api-keys-and-mcp.md) for the key):

```sh
export MINION_HUB_URL=https://hub.example.com
export MINION_API_KEY_FILE=$HOME/.config/minion-api-key

minion pipe_send --target <worker-id>:<session> --text 'Run the tests and tell me what fails.'
minion pipe_wait --target <worker-id>:<session> --timeout-ms 120000
minion pipe_read --target <worker-id>:<session>
```

Without a Hub, leave the Worker out of the target (`--target <session>`): the commands act on the local Worker.

- `pipe_send` always presses Enter after the message, in the same request, and the agent receives it as a prompt. The text is pasted verbatim; Tab and line feed are allowed and other control characters are refused. A message is at most 8 KiB.
- `pipe_wait` streams the next turns as they arrive and ends when the timeout passes; `--after-turn-id` continues after a turn you already have. `pipe_read` returns the stored entries, the messages you sent and the replies, in the order the Hub committed them, a page at a time; the `turn_id` of the last entry is the `--after-turn-id` of the next call.
- The Hub is the broker: it checks the right to send (`message` or `operate` on the session), limits the rate (one message per second per session with a burst of five, and 120 per hour) and a **task budget** of messages per session per day (setting `pipe_budget_per_day`, default 200, 1 to 100000), and it never queues a refused message.
- A send has a request id like every write: repeating the same `--request-id` within 10 minutes never delivers twice, and a result of `unknown` after a broken connection means the message may have arrived (see [API keys](api-keys-and-mcp.md#use-a-key)).

## Agent to agent: the call key and `pipe_accept_from`

A Worker, and the agent or MCP server on it, can talk to **its own** sessions without any setup. To message a session on **another** Worker, even one of your own, two things must be in place, because a compromised Worker or a prompt-injected agent must not be able to type into every other machine you own:

1. **The sending Worker holds a call key.** Create an API key in the web UI bound to the sending Worker (the "Call key for a Worker" choice of the API keys form, see [API keys](api-keys-and-mcp.md#let-a-worker-call-other-workers)) with `message` on only the target Workers or sessions, save it in a file with mode 0600 and set `call_api_key_file` on the sending Worker. Without the key the Hub gives a Worker identity access to its own Worker and its sessions and to nothing else; another Worker looks like a target that does not exist.
2. **The receiving Worker accepts the sender.** Set `pipe_accept_from` on the receiving Worker:
   - `none` (the default): messages from other Workers are refused. Messages from people and from keys that are not bound to a Worker are not affected, and a Worker can always message itself.
   - `owner-workers`: accept Workers of the same owner as this Worker.
   - a comma-separated list of Worker ids or fingerprints: accept exactly those.

```sh
# on the receiving Worker (restart the Worker afterwards)
minion worker config set pipe_accept_from owner-workers
# on the sending Worker
minion worker config set call_api_key_file /home/me/.config/minion-call-key
```

`pipe_accept_from` is a local setting. No Hub operation, key or administrator can change it; only someone with access to the Worker's own configuration can. A refused message is answered `pipe_origin_refused`, nothing reaches the session, and the Worker's local audit records the refusal with the origin the Hub reported. The same rule covers `send_text`, `send_keys`, `answer` and `set_task`, not only `pipe_send`. The Hub derives the origin from the authenticated caller (never from the request), and a Worker refuses a message that arrives without one.

The honest limit: this protects each Worker against a compromised **peer Worker** (or its agent) while the Hub is honest. A compromised Hub or administrator can still reach the sessions it may reach ([security](../security.md)).

## When no reply turn appears

A message is delivered whether or not a reply can be recorded. The Worker records a turn only when it can prove which part of the screen is the reply, and otherwise records none rather than guess. No turn appears:

- **before tmux 3.8**: older tmux gives no row counters (the Worker's log says `tmux_too_old`). Sessions still list, read and receive messages; only the reply capture is off. Use tmux 3.8 or newer on the Worker.
- **on Windows** (`no_evidence`): the preview host gives no such evidence.
- **for agents that use the terminal's alternate screen** (`alternate_screen`): some agents do, or can be started so that they do not; for example Codex only with `--no-alt-screen`.
- when the pane was **resized, cleared, scrolled out of range or replaced** between the message and the reply, or the agent printed its transcript again: the Worker's log names the reason, with the session name and never the content.

The pipe is not a chat built from screen scraping: only turns that were recorded with that evidence exist, and a session without them simply has none.

## Who sees what, and trust

- A person in the browser sees every turn they may read.
- A **model** (a caller with an API key, or a Worker identity) gets received turns only from sources its user has **trusted**. `pipe_read` answers with the turns that remain, the number of withheld turns and the reason `untrusted_peer`; `pipe_wait` on an untrusted source ends at once with the error `untrusted_peer`, and one that loses trust while it waits ends the same way. Mark a Worker or a session as trusted for yourself with `trust_set`, and list the marks with `trust_list`. Trust never restricts a person.
- Replies come from an agent, and an agent's output is whatever the agent, or the page, file or message it read, made it say. Do not treat a reply as an instruction from a person; that is why a model reads only trusted sources.
- Stored pipe history belongs to the sharing rule: when a session stops being shared, the Hub stops serving its history, open waits end with `not_shared` and a wait that has already fetched several turns delivers no further one; sharing it again serves what was kept. See [pairing and sharing](pairing.md#choosing-what-the-hub-sees).

## Limits worth knowing

Turns and messages are stored in the Hub's database, within quotas per Worker, per owner and for the whole Hub; a Worker that keeps flooding the turn channel after the Hub told it to wait loses its connection. The Hub stamps turns with its own clock. A message and its reply are visible to the Hub like everything else a Worker shares ([security](../security.md)).
