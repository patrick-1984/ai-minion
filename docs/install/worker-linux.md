# Run a Worker on Linux and Raspberry Pi

Back to the [README](../../README.md).

A Worker serves the tmux sessions of one Unix user on one machine. It needs `tmux` (any version works for sessions; replies of agents through [the pipe](../guides/pipe.md) need tmux 3.8 or newer). The Worker never renames, closes or changes a session on its own. One Worker per Unix user: it sees the tmux server of the user it runs as.

## 1. Install the binary

One line installs the latest `minion` into `/usr/local/bin` (when writable, for example as root) or `~/.local/bin`, after it checked the archive against `checksums.txt` of the release: `curl -fsSL https://github.com/patrick-1984/ai-minion/releases/latest/download/install.sh | sh`. Install `tmux` with your package manager. The packages and the verified manual install below are the alternatives.

Download from `https://github.com/patrick-1984/ai-minion/releases` the file for your architecture. Check the release page for the exact names; the packages end in the architecture of your machine, and the archives are named `minion_<version>_linux_<arch>.tar.gz` with `amd64`, `arm64` or `armv7`.

**Debian, Ubuntu, Raspberry Pi OS (deb):**

```sh
sudo apt install ./minion_<version>_<arch>.deb
```

**Fedora, RHEL and relatives (rpm):**

```sh
sudo dnf install ./minion-<version>-1.<arch>.rpm
```

Both packages install `/usr/bin/minion` with `mn` as a second name, depend on `tmux`, and add the systemd unit template `minion-worker@.service`. They do not start or enable a Worker: you choose the user in the next step.

**From the archive (any Linux):** verify and install the archive exactly as in [the Hub guide](hub-binary.md#get-the-binary), install `tmux` with your package manager, and put `minion` in a directory on the `PATH`:

```sh
sudo install -m 0755 minion /usr/local/bin/minion
```

**Raspberry Pi:** use the `arm64` build on a 64-bit OS and the `armv7` build on a 32-bit OS (`uname -m` prints `aarch64` or `armv7l`). Install tmux first (`sudo apt install tmux`). Everything else is the same as on any Linux machine.

## 2. Set the Worker up

Run this as the Unix user whose tmux sessions you want to serve, in a terminal:

```sh
minion worker setup
```

The wizard asks three things in turn: which sessions the Hub may see (all of them, some, decide later, or none), the Hub address (it checks the name, the connection, the certificate and the protocol and explains each result) and a pairing code to type into the Hub. Nothing is saved before the Hub address step is finished, and Ctrl-C before that changes nothing. Pressing Enter at the address step runs the Worker alone (Solo).

Everything the wizard asks can be given as a flag instead, for example `--share-existing none --server https://hub.example.com`. A question answered that way is not asked. Without a terminal the wizard asks nothing: it needs `--share-existing`, and it does not pair.

## 3. Run it as a service

**System service (sudo):**

```sh
sudo minion worker up --share-existing none --server https://hub.example.com
```
Nothing is shared at first (`--share-existing none`): choose the sessions to share on the Worker's page in the Hub (**Sharing**: tick **Shared**, or **All** / **None**). The flag is saved and applied at every start, so once you share from the Hub remove it with `minion worker config unset share_existing`, or that choice is reset.

Run by a user through `sudo`, this installs `minion-worker@<user>.service` for that user, enables it at boot and starts it. The unit runs `minion worker run` as that user (`Restart=always`, `KillMode=process`, so stopping or restarting the Worker does not end your tmux sessions). Use `--as-user <name>` to install it for another user; a first-run question is then not asked for you: give `--share-existing all|none|later` or run `minion worker setup` as that user.

You can also enable the shipped unit yourself after the Worker has been set up:

```sh
sudo systemctl enable --now minion-worker@<user>
```

**User service (no sudo):**

```sh
minion worker up --share-existing all --server https://hub.example.com
```

Without `sudo` this installs a systemd user service. A user service stops when you log out unless lingering is on. The command turns lingering on for you (`loginctl enable-linger`); when the system does not allow that, it prints the exact command to run as an administrator and exits with an error.

When the system service `minion-worker@<user>.service` of your account is already installed (enabled, running, or configured by an earlier `sudo minion worker up`), `minion worker up` without `sudo` refuses to add a user service beside it: it names the unit, changes nothing and exits with status 1. Run `sudo minion worker up` to change the system service. The unit template that the deb and rpm packages ship (`minion-worker@.service`) does not count as an installed Worker.

`minion worker up` writes the flags you give, and the settings of any `MINION_*` variables, to the Worker's config file in the state directory so that the service uses them. Secrets are never written there. The state directory is `~/.local/state/minion` unless `--state-dir`, `MINION_STATE_DIR` or `XDG_STATE_HOME` says otherwise.

**`--share-existing` is applied at every start.** `minion worker up` saves the flag in the config file, and the Worker applies a `share_existing` value to its saved choice each time it starts. A choice you make later in the Hub is therefore replaced at the next restart. To let the Hub's choice stand, give the flag for the first run and then run `minion worker config unset share_existing`: the saved choice is then used. `minion worker config share ...` keeps the config file in step, so it never gets undone. See [pairing and sharing](../guides/pairing.md#who-decides-at-start).

**Visible sessions under systemd.** A service does not inherit your shell's environment. With `sessions_visible` at `auto` (the default) the Worker judges the tmux socket as seen by the service: its default socket is `/tmp/tmux-<uid>/default` unless the unit sets `TMUX_TMPDIR`. If your shell sets another `TMUX_TMPDIR` (or you use `tmux -S`/`-L`), the service's tmux server is not the one your `tmux ls` shows, although the Worker reports it as visible. Then point the Worker at your socket (`minion worker config set tmux_socket <path>`) and say what is true with `minion worker config set sessions_visible yes` (your own socket) or `no` (a separate server), and restart the service.

**In the foreground** (to try it, or under another supervisor): `minion worker up --foreground`, or `minion worker run`, until Ctrl-C.

## 4. Join the Hub

The quickest way: in the Hub open **Workers**, **Add worker**, **Add a Worker in one step**, and run the command it shows: `minion worker up --join mnj1-...` as the user whose sessions you serve (a user service; in a terminal it asks what to share), or the page's **system service** variant, which runs `sudo env MINION_JOIN_FILE=<private file> minion worker up --share-existing later` so that the token is neither on sudo's command line nor in its log. Either enrolls the Worker with a one-time key and connects only to the Hub with the fingerprint in the token ([add a Worker in one step](../guides/pairing.md#add-a-worker-in-one-step)).

If the wizard did not already pair the Worker, pair it now: [pairing and sharing](../guides/pairing.md). The wizard and `minion worker pair` ask for the Hub fingerprint first: read it on the Hub host with `minion hub identity --words`, or give it as `--hub-fingerprint` ([pairing](../guides/pairing.md#give-the-worker-the-hub-fingerprint)).

## Check and change

```sh
minion worker status                          # mode, Hub, connection, fingerprints, sharing, tmux
minion worker config share all                # all, none or later
minion worker config share select work build  # only these sessions
minion worker config set server https://hub.example.com
sudo systemctl restart minion-worker@<user>   # after a setting other than the share choice
```

The share choice is read from the Worker's database and takes effect at once; the Hub follows it within the Worker's poll interval (one second by default, setting `session_poll_interval`). Any other saved setting is used the next time the Worker starts. A flag or an environment variable still wins over the config file.

To diagnose a problem, run the Worker with `--debug` (or `MINION_DEBUG=1`). To let its owner or an administrator switch debug logging on and read its recent log from the Hub's Worker page, start it with `--allow-debug-remote` (or `minion worker config set allow_debug_remote true` and restart it); it is off by default. See [troubleshooting](../guides/troubleshooting.md#worker-debug---debug-and-minion_debug).

### Block new sessions created through Minion

```sh
minion worker config set allow_new_sessions off   # block
minion worker config set allow_new_sessions on    # allow again (default)
```

With `allow_new_sessions` off the Worker refuses every new session created through Minion, whoever asks: the Hub, an API key, local commands, `mn` and MCP get `forbidden` with the reason `new_sessions_blocked`. Existing sessions keep working, and so do the other writes. The block is local and tighten-only: `off` from any source (the config file, `MINION_ALLOW_NEW_SESSIONS` or `--allow-new-sessions`), or a value that is not `on` or `off`, blocks, so an `on` in a service environment cannot lift a block saved in the config file, and `minion worker up` keeps a saved `off`. The Hub cannot lift it. `minion worker status` shows it as `new_sessions` (`blocked`, the `sources` that block, and `in_force`). Limit: a writable shared terminal can still start programs or sessions by typing in it; for a stronger switch use `preview_only` or share fewer sessions. Restart the Worker after a change.

### Pause the Hub connection

```sh
minion worker pause            # until minion worker resume
minion worker pause --for 30m  # ends by itself after 30 minutes
minion worker resume
```

A paused Worker drops its Hub connection and does not connect again until you resume it or the `--for` time is over; it stays paused across a restart (the pause is saved as `pause.json` in the state directory). Your sessions keep running and stay usable on this machine; nothing ends. Writes that the Hub started before the pause finish (at most 30 seconds are waited for them) and the Hub reads their results after the resume. While paused, `minion worker status` shows `Paused`, and commands that need the Hub (for example `minion workers_list`) answer `paused`. A paused Worker also does not pair, enroll with an invitation key or revoke a replaced identity at its Hub: a start while paused defers that work, and it runs right after the resume, without a restart. `minion worker pair` and `minion worker join` refuse with exit status 25 while the Worker is paused. A Worker without a Hub (Solo) has nothing to pause. With `--json` both commands print one JSON object; a running Worker of an older version that cannot pause refuses with exit status 20 and nothing changes.

## Several Workers on one machine

Run one Worker per Unix user. For test setups with more than one Worker of the same user, give each its own `MINION_STATE_DIR`, `MINION_TMUX_SOCKET` and `XDG_RUNTIME_DIR`: the command line finds the Worker through the control socket `worker.sock` in the runtime directory, and two Workers that share one runtime directory share one socket path. A Worker started by a system service has no `XDG_RUNTIME_DIR`; the commands look for its socket in the per-user directory under the temporary directory as well.

## Update

```sh
sudo minion update                  # the newest release
sudo minion update --channel 0.6    # the newest 0.6.x
sudo systemctl restart minion-worker@<user>
```

`minion update` verifies the release signature and checksum first and refuses an update whose protocol range does not overlap the Hub the Worker is connected to. With the packages, `apt install` or `dnf install` of a newer file also restarts the Worker instances that already run.

## Stop, reset and remove

```sh
sudo systemctl disable --now minion-worker@<user>
minion worker down           # as that user: stops the running Worker (or prints the systemctl command a system service needs)
minion worker reset          # as that user: forgets the Hub, the key, the enrollment and the share choice
```

`minion worker reset` refuses while a Worker runs (stop it with `minion worker down`) and does not touch your tmux sessions. It does not tell the Hub: remove the Worker in the Hub's Workers page as well, because pairing again creates a new Worker with a new key. Then remove the package (`apt remove minion`, `dnf remove minion`) if you want the binary gone.
