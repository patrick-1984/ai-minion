# Run a Worker on Windows (preview)

Back to the [README](../../README.md).

The Windows Worker is a **preview** in release 0.8.2. It works differently from the Linux and macOS Worker, and some things are missing. Read the limits before you rely on it.

## What it is

Windows has no tmux. The Worker creates sessions as ConPTY (pseudo console) programs that live **inside the Worker process**. The same operations work on them: list, snapshot, capture, send text and keys, set a task, create a session, the Hub pages and the MCP tools.

## Limits of the preview

- **Sessions end with the Worker.** There is no server that outlives the Worker. Restarting or stopping the Worker ends every session, and the session list is empty afterwards. Sessions are not saved and not adopted again.
- **No replies through the pipe.** A message sent with `pipe_send` is delivered, but the Windows Worker extracts no agent reply from the screen (the reason reported is `no_evidence`). See [the pipe](../guides/pipe.md).
- **The service runs as LocalSystem.** When you install the Worker as a Windows service, the sessions it creates run as that account, not as you. To run sessions as yourself, stop the service and run `minion worker up --foreground` in your own terminal.
- The Windows build has had less testing than the Linux build. The pairing handshake has not had an independent cryptographic review yet (see [security](../security.md)); that applies to every platform.

## Install

One line in PowerShell (Windows PowerShell 5.1 or PowerShell 7) installs `minion.exe` into `%LOCALAPPDATA%\Programs\minion` and adds that directory to your user `PATH`; it checks the zip against `checksums.txt` of the release:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; irm https://github.com/patrick-1984/ai-minion/releases/latest/download/install.ps1 | iex
```

Or install it by hand and verify the signature too: download `minion_<version>_windows_amd64_preview.zip` (or the `arm64` zip on a Windows-on-Arm machine) from `https://github.com/patrick-1984/ai-minion/releases`. Check its SHA-256 against `checksums.txt` of the same release, for example in PowerShell:

```powershell
Get-FileHash .\minion_<version>_windows_amd64_preview.zip -Algorithm SHA256
```

Compare the result with the line for that file in `checksums.txt`. (The signature of `checksums.txt` is verified as in [the Hub guide](hub-binary.md#get-the-binary) with `cosign`, if you have it.) Unpack the zip and put `minion.exe` in a directory on your `PATH`.

## Pair and run it

In a terminal (PowerShell or the Windows Terminal), as the user whose sessions you want to serve:

```powershell
minion worker up --foreground --share-existing none --server https://hub.example.com
```
Nothing is shared at first (`--share-existing none`): choose the sessions to share on the Worker's page in the Hub (**Sharing**: tick **Shared**, or **All** / **None**). The flag is saved and applied at every start, so once you share from the Hub remove it with `minion worker config unset share_existing`, or that choice is reset.

A Worker that is not paired yet shows a code like `MN-K7Q4-XY12` (on the console and in the log) and waits. In the Hub open **Workers**, choose **Add worker** and type the code. The Worker then connects by itself and is **pending**: its log says `this Worker is waiting for approval`. Approve it in the Hub (**Workers**, compare the fingerprint, **Approve**) or with `minion hub worker approve <fingerprint|id>`. The Worker runs until you press Ctrl-C. Details: [pairing and sharing](../guides/pairing.md).

Instead of the code, an invitation key from the Hub (**Invitations**) enrolls the Worker: add `--invite-key -` and paste the key on standard input (other forms: [the invitation key](../guides/pairing.md#enroll-with-an-invitation-key)). The Worker is pending until you approve it, as above. `minion worker pair --server <url>` still pairs without starting the Worker.

`--share-existing` has no tmux sessions to count on Windows; it decides what the Hub may see of the sessions you create.

To install it as a Windows service instead, pair first (run the command above until the Worker is paired, or `minion worker pair`), then run the same `worker up` command from an elevated terminal without `--foreground`. The service uses the state directory of the user who installs it, so it keeps that pairing. The command prints a note about the LocalSystem account when it installs the service. The service starts at boot and is restarted five seconds after a failure; the command line reaches it over a named pipe that only the installing user and the system may open.

## Check and reset

```powershell
minion worker status
minion worker config share all
minion worker reset
```

`minion worker reset` forgets the Hub, the key and the share choice and refuses while the Worker runs. It does not tell the Hub: remove the Worker on the Hub's Workers page too.
