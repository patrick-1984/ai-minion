# Run a Worker on Linux and Raspberry Pi

Back to the [README](../../README.md).

A Worker serves the tmux sessions of one Unix user on one machine. It needs `tmux` (any version works for sessions; replies of agents through [the pipe](../guides/pipe.md) need tmux 3.8 or newer). The Worker never renames, closes or changes a session on its own. One Worker per Unix user: it sees the tmux server of the user it runs as.

## 1. Install the binary

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

Everything the wizard asks can be given as a flag instead, for example `--share-existing all --server https://hub.example.com`. A question answered that way is not asked. Without a terminal the wizard asks nothing: it needs `--share-existing`, and it does not pair.

## 3. Run it as a service

**System service (sudo):**

```sh
sudo minion worker up --share-existing all --server https://hub.example.com
```

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

`minion worker up` writes the flags you give, and the settings of any `MINION_*` variables, to the Worker's config file in the state directory so that the service uses them. Secrets are never written there. The state directory is `~/.local/state/minion` unless `--state-dir`, `MINION_STATE_DIR` or `XDG_STATE_HOME` says otherwise.

**`--share-existing` is applied at every start.** `minion worker up` saves the flag in the config file, and the Worker applies a `share_existing` value to its saved choice each time it starts. A choice you make later in the Hub is therefore replaced at the next restart. To let the Hub's choice stand, give the flag for the first run and then run `minion worker config unset share_existing`: the saved choice is then used. `minion worker config share ...` keeps the config file in step, so it never gets undone. See [pairing and sharing](../guides/pairing.md#who-decides-at-start).

**In the foreground** (to try it, or under another supervisor): `minion worker up --foreground`, or `minion worker run`, until Ctrl-C.

## 4. Join the Hub

If the wizard did not already pair the Worker, pair it now: [pairing and sharing](../guides/pairing.md).

## Check and change

```sh
minion worker status                          # mode, Hub, connection, fingerprints, sharing, tmux
minion worker config share all                # all, none or later
minion worker config share select work build  # only these sessions
minion worker config set server https://hub.example.com
sudo systemctl restart minion-worker@<user>   # after a setting other than the share choice
```

The share choice is read from the Worker's database and takes effect at once; the Hub follows it within the Worker's poll interval (one second by default, setting `session_poll_interval`). Any other saved setting is used the next time the Worker starts. A flag or an environment variable still wins over the config file.

## Several Workers on one machine

Run one Worker per Unix user. For test setups with more than one Worker of the same user, give each its own `MINION_STATE_DIR`, `MINION_TMUX_SOCKET` and `XDG_RUNTIME_DIR`: the command line finds the Worker through the control socket `worker.sock` in the runtime directory, and two Workers that share one runtime directory share one socket path. A Worker started by a system service has no `XDG_RUNTIME_DIR`; the commands look for its socket in the per-user directory under the temporary directory as well.

## Update

```sh
sudo minion update                  # the newest release
sudo minion update --channel 0.5    # the newest 0.5.x
sudo systemctl restart minion-worker@<user>
```

`minion update` verifies the release signature and checksum first and refuses an update whose protocol range does not overlap the Hub the Worker is connected to. With the packages, `apt install` or `dnf install` of a newer file also restarts the Worker instances that already run.

## Stop, reset and remove

```sh
sudo systemctl disable --now minion-worker@<user>
minion worker reset          # as that user: forgets the Hub, the key, the enrollment and the share choice
```

`minion worker reset` refuses while a Worker runs and does not touch your tmux sessions. It does not tell the Hub: remove the Worker in the Hub's Workers page as well, because pairing again creates a new Worker with a new key. Then remove the package (`apt remove minion`, `dnf remove minion`) if you want the binary gone.
