# Run a Worker on Linux and Raspberry Pi

Back to the [README](../../README.md).

A Worker serves the tmux sessions of one Unix user on one machine. It needs `tmux` (3.0 or newer for sessions; replies of agents through [the pipe](../guides/pipe.md) need tmux 3.8 or newer). The Worker never renames, closes or changes a session on its own. One Worker per Unix user: it sees the tmux server of the user it runs as.

## 1. Install the binary

One line installs the latest `minion` into `/usr/local/bin` (when writable, for example as root) or `~/.local/bin`, after it checked the archive against `checksums.txt` of the release:

```sh
curl -fsSL https://github.com/patrick-1984/ai-minion/releases/latest/download/install.sh | sh
```

The installer uses your package manager and, when needed, noninteractive sudo to install a missing tmux. The packages and the verified manual install below are the alternatives.

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

**Raspberry Pi:** use the `arm64` build on a 64-bit OS and the `armv7` build on a 32-bit OS (`uname -m` prints `aarch64` or `armv7l`). The same installer installs tmux on the Pi. Everything else is the same as on any Linux machine.

### Upgrading an old tmux

Minion needs tmux 3.0 or newer, so Ubuntu 20.04's tmux 3.0a is fine (keep it updated with the distribution's security updates). Install a newer tmux from source only when the distribution offers something older than 3.0, or when you want the tmux release used in Minion CI (the pipe needs 3.8). On Debian or Ubuntu:

```sh
sudo apt-get update && sudo apt-get install -y build-essential bison pkg-config libevent-dev libncurses-dev curl
(
  set -eu
  build_dir=$(mktemp -d)
  trap 'rm -rf "$build_dir"' EXIT
  cd "$build_dir"
  curl -fsSLO https://github.com/tmux/tmux/releases/download/3.8/tmux-3.8.tar.gz
  echo 'e79c699c7e949dccd0a4a125e17b8d1261e311b16979dbc8eb34542f3966d82e  tmux-3.8.tar.gz' | sha256sum -c -
  tar xzf tmux-3.8.tar.gz
  cd tmux-3.8
  ./configure --prefix=/usr/local
  make && sudo make install
)
hash -r
tmux -V
```

Then rerun the Hub's command. Existing tmux sessions belong to the server version that started them. Save their work and restart that server at a convenient time if it reports a client/server version mismatch.

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

Run by a user through `sudo`, this installs `minion-worker@<user>.service` for that user, enables it at boot and starts it. The unit runs `minion worker run` as that user (`Restart=always`, `KillMode=process`, so stopping or restarting the Worker does not end your tmux sessions). Use `--as-user <name>` to install it for another user. In a terminal, the setup asks what to share for that user. An unattended installation can give `--share-existing all|none|later`.

You can also enable the shipped unit yourself after the Worker has been set up:

```sh
sudo systemctl enable --now minion-worker@<user>
```

**User service (no sudo):**

```sh
minion worker up --share-existing all --server https://hub.example.com
```

Without `sudo` this installs a systemd user service. A user service stops when you log out unless lingering is on. The command turns lingering on for you (`loginctl enable-linger`); when the system does not allow that, it prints the exact command to run as an administrator and exits with an error.

`minion worker up` writes the flags you give, and the settings of any `MINION_*` variables, to the Worker's config file in the state directory so that the service uses them. Secrets are never written there. The state directory is `~/.local/state/minion` unless `--state-dir`, `MINION_STATE_DIR` or `XDG_STATE_HOME` says otherwise.

**`--share-existing` is applied at every start.** `minion worker up` saves the flag in the config file, and the Worker applies a `share_existing` value to its saved choice each time it starts. A choice you make later in the Hub is therefore replaced at the next restart. To let the Hub's choice stand, give the flag for the first run and then run `minion worker config unset share_existing`: the saved choice is then used. `minion worker config share ...` keeps the config file in step, so it never gets undone. See [pairing and sharing](../guides/pairing.md#who-decides-at-start).

**Visible sessions under systemd.** A service does not inherit your shell's environment. With `sessions_visible` at `auto` (the default) the Worker judges the tmux socket as seen by the service: its default socket is `/tmp/tmux-<uid>/default` unless the unit sets `TMUX_TMPDIR`. If your shell sets another `TMUX_TMPDIR` (or you use `tmux -S`/`-L`), the service's tmux server is not the one your `tmux ls` shows, although the Worker reports it as visible. Then point the Worker at your socket (`minion worker config set tmux_socket <path>`) and say what is true with `minion worker config set sessions_visible yes` (your own socket) or `no` (a separate server), and restart the service.

**Without systemd** (including Coder workspaces), the same `minion worker up` command starts a detached Worker and returns the prompt. Its private log is `worker-launch.log` in the state directory. `minion worker down` stops it, and `minion worker start` starts it again. Login startup uses marked blocks in `~/.profile`, `~/.bashrc` and the active Bash login file if present. On zsh it also uses `~/.zprofile`. The blocks read the executable path from `~/.config/minion/worker-login`, which also records the state directory. The block starts the Worker in the background without creating a shell job, so interactive shells print no `[1]` or `Done` lines, and it runs once per shell even when `~/.profile` sources `~/.bashrc`. Once a Worker has started, it updates the blocks that earlier releases wrote (v0.20.5 and older), changing only the exact managed block between the complete `# Minion Worker login startup` marker lines; a block you edited is left as it is. Before it changes a file it saves the original as `<file>.minion-backup` next to it (`.minion-backup.1` and so on if that exists; delete them once you are satisfied), and it changes the file in place, keeping its owner, group, mode and links. A startup file that is a symbolic link, or that changed while it was updated, is left as it is and named in the Worker log. To disable shell startup, remove that registration file. Use the Docker instructions if you want Docker to restart the Worker when the container starts.

**In the foreground** (to try it, or under another supervisor), until Ctrl-C:

```sh
minion worker up --foreground
```

`minion worker run` does the same without the setup steps.

## 4. Join the Hub

The quickest way to join is **Workers**, **Add worker**, **Add a Worker in one step**. Run the Linux command in the terminal of the user whose sessions you want to share. Use the sudo command for a system service. On its first interactive run, the sudo command offers to serve the invoking user’s sessions or root’s sessions. Both commands start the Worker in the background. Compare the words in the startup summary and approve it on the Workers page. [Add a Worker in one step](../guides/pairing.md#add-a-worker-in-one-step).

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

## Start, pause, block new sessions, choose sessions

`minion worker start` starts the installed Worker (it never installs anything); `minion worker pause` and `resume` take the Worker off the Hub and back without stopping your sessions; `minion worker config set allow_new_sessions off` blocks new sessions from every origin. `minion worker up --join -` reads the join token from standard input. All of them are described in [Control a Worker from the command line](../guides/worker-control.md).

Other programs on this machine can check that the Worker runs and which version it is: [read the Worker status from another program](../guides/integrate-local.md).

## Several Workers on one machine

Run one Worker per Unix user. For test setups with more than one Worker of the same user, give each its own `MINION_STATE_DIR`, `MINION_TMUX_SOCKET`, `XDG_RUNTIME_DIR` and `TMPDIR`: the command line finds the Worker through the control socket `worker.sock` in the runtime directory, and two Workers that share one runtime directory share one socket path. A Worker started by a system service has no `XDG_RUNTIME_DIR`; the commands look for its socket in the per-user directory under the temporary directory as well.

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
