# Control a Worker from the command line

Commands for the Worker of your own user. Each works on Linux, macOS and Windows and is what a tray menu runs; none needs administrator rights.

| Command | What it does |
| --- | --- |
| `minion worker start` | Starts the installed Worker through its manager: the logon supervisor on Windows, the systemd unit or shell login registration on Linux, the launchd agent on macOS. A Worker that runs is left alone. Nothing installed: it says so and points to `minion worker up`. Only a unit that `worker up` enabled counts; the packaged template alone is not a Worker. On Windows it refuses a process with administrator rights (an elevated prompt, or an administrator whose token is not filtered); run it from a normal prompt. A system-wide systemd unit needs `sudo systemctl start minion-worker@<user>`; the command prints it. |
| `minion worker down` | Stops it (existing command). Open terminals of ConPTY on Windows end with the Worker. |
| `minion worker pause` | Until `resume`: the Hub link is closed and the Worker makes no Hub request (no reconnect, no automatic pairing, no request that needs the Hub from `mn` or MCP). The command reports success only after that is true. Local control and the terminal sessions that are open keep working. The pause is saved, so a restart keeps it; a pause file that cannot be read keeps the Worker paused. Only a running, enrolled Worker pauses (it saves the choice in the profile it serves): with none running, or one not enrolled yet, the command fails and says what to do first. |
| `minion worker resume` | Connects again. A Worker older than this release cannot pause; update it and restart it. |
| `minion worker config set allow_new_sessions off` (or `on`) | Blocks every new session made through Minion: the Hub, an API key, `mn`, MCP and local commands get `forbidden` / `new_sessions_blocked`. Existing sessions and other writes keep working. The Hub cannot lift it. `off` in the config file also holds against an `on` in the environment or a flag; a value that is not `on` or `off` blocks. A running Worker enforces the saved value at once. Limit: someone who may type in a writable shared terminal can still start programs there. |
| `minion worker status [--json]` | Shows `paused` and `new_sessions` (`on`, `off`, or `unknown` when a running Worker cannot confirm it, for example an older one). |
| `minion worker up --join -` | Reads the join token from the first line of standard input (at most 4096 bytes), so it is in no argument list; `--join <token>`, `MINION_JOIN` and `MINION_JOIN_FILE` still work. The Hub pin and invitation checks are the same. Only the way the token is read changes: sharing behaves as with `--join <token>` (without a terminal on a fresh profile, give `--share-existing` or an initial `--non-interactive-share` choice). The token and the key inside it are never printed, logged or saved. A paused Worker refuses the join (resume first), and an enrollment dial re-reads the pause right before it. `worker up` saves `allow_new_sessions` only when given by flag or environment and never overwrites a saved value otherwise. |

| `minion worker discover [--json]` | Lists the Workers this account can see on the machine (its own and root's) with status `running`, `stale`, `stopped` or `unavailable`, the daemon version and the scope, after verifying every record. TODO(R7): confirm name, flags and output. See [integrate with local programs](integrate-local.md). |

Pairing with a code stays `minion worker pair` or `minion worker join <url>` in a terminal.

Back to the [README](../../README.md).
