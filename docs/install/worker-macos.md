# Run a Worker on macOS

Back to the [README](../../README.md).

A Worker serves the tmux sessions of the macOS user who runs it. It needs `tmux`. The Worker never renames, closes or changes a session on its own. There is no macOS package: you install the `minion` binary from the release archive.

## 1. Install tmux and the binary

```sh
brew install tmux
```

Download the archive for your Mac from `https://github.com/patrick-1984/ai-minion/releases`: `minion_<version>_darwin_arm64.tar.gz` for Apple silicon, `minion_<version>_darwin_amd64.tar.gz` for Intel. Verify it as described in [the Hub guide](hub-binary.md#get-the-binary) (use `shasum -a 256 -c` where `sha256sum` is not installed), then:

```sh
tar -xzf minion_<version>_darwin_arm64.tar.gz minion
sudo install -m 0755 minion /usr/local/bin/minion
minion version
```

If macOS refuses to open a binary that a browser downloaded, remove the quarantine mark of that file (`xattr -d com.apple.quarantine minion`) after you have verified it. A file fetched with `curl` carries no mark.

## 2. Set the Worker up and keep it running

Run this as yourself, in a terminal:

```sh
minion worker setup
```

The wizard asks which sessions the Hub may see, the Hub address and a pairing code (see [pairing and sharing](../guides/pairing.md)). To skip the questions, give them as flags:

```sh
minion worker up --share-existing all --server https://hub.example.com
```

On macOS `minion worker up` installs a launchd agent for the current user and loads it, so the Worker starts when you log in and runs `minion worker run`. The Worker's state (database, config file, logs) is in `~/Library/Application Support/minion`. A launchd user agent runs while you are logged in; it does not run at the login window.

**`--share-existing` is applied at every start.** `minion worker up` saves the flag in the config file, and the Worker applies a `share_existing` value to its saved choice each time it starts. A choice you make later in the Hub is therefore replaced at the next restart. To let the Hub's choice stand, give the flag for the first run and then run `minion worker config unset share_existing`: the saved choice is then used. `minion worker config share ...` keeps the config file in step, so it never gets undone. See [pairing and sharing](../guides/pairing.md#who-decides-at-start).

Run it in the foreground instead (to try it) with `minion worker up --foreground`.

## Check and change

```sh
minion worker status
minion worker config share all                # all, none or later
minion worker config share select work build  # only these sessions
```

The share choice takes effect at once; the Hub follows within the poll interval (one second by default). Any other saved setting is used at the next start of the Worker, so restart it after you change one (log out and in again, or stop and start its launchd agent).

## Update

```sh
sudo minion update
```

`minion update` verifies the signature and checksum of the release before it replaces the binary. Restart the Worker afterwards so that it runs the new binary (log out and in again, or stop and start its launchd agent).

## Stop and remove

`minion worker reset` forgets the Hub, the key and the share choice on this Mac and does not touch your tmux sessions. It refuses while the Worker runs, so stop the agent first. It does not tell the Hub: remove the Worker on the Hub's Workers page too.

## Docker Desktop is not a way to run this Worker

A Worker in a Docker container on macOS runs inside a Linux virtual machine and cannot reach the tmux server of your Mac. Run the Worker natively as above.
