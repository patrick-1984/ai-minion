# Run a Worker on Windows (preview)

Back to the [README](../../README.md).

The Windows Worker is a **preview** in release 0.10.0. It works differently from the Linux and macOS Worker, and some things are missing. Read the limits before you rely on it.

## What it is

**With psmux (0.9.2 and later).** [psmux](https://github.com/psmux/psmux) is tmux for Windows. When psmux 3.4.0 or later is installed and the Worker runs as you (`minion worker up --foreground`), the Worker serves **your psmux sessions**: the ones you start with `psmux new -s work` show up in the Hub, and the sessions the Worker creates are psmux sessions you can `psmux attach` to. They keep running when the Worker stops, restarts or updates. On the first start in a console without psmux, the Worker asks once whether to install it with `winget install psmux` (the answer is kept in `psmux_offer`; `minion worker config set psmux_offer ask` asks again). `minion worker config set session_host conpty` (or `psmux`) chooses explicitly; the default `auto` uses psmux when it is installed and the Worker is not a Windows service.

Windows terminals that are not psmux sessions (a plain PowerShell or cmd window) are not visible: run the work you want to reach from the Hub inside psmux.

**Without psmux.** The Worker creates sessions as ConPTY (pseudo console) programs that live **inside the Worker process**. The same operations work on them: list, snapshot, capture, send text and keys, set a task, create a session, the Hub pages and the MCP tools.

## Visible and hidden sessions

A session is **visible** when you can see and join it from your own terminal: psmux sessions in your default namespace show in `psmux ls` and you can `psmux attach` to them. A session is **hidden** when you cannot: a ConPTY session lives inside the Worker process (psmux is not installed, `session_host` is `conpty`, or the Worker runs as a Windows service as LocalSystem). The same holds on Linux and macOS: sessions on your default tmux server are visible, sessions on a private socket (`tmux_socket`), in the tmux server of root under a service manager, or on the tmux server inside the Minion image are hidden (a `minion` installed by hand inside a container you work in, such as a Coder workspace, serves your own tmux server there: visible). The Worker setting `sessions_visible` (`auto`, `yes`, `no`; Linux and macOS) overrides that guess, for example `yes` for a container that mounts your own tmux socket ([Docker](worker-docker.md)).

The Worker reports its session host and the reason to the Hub. The Worker page and the Workers list show it ("Sessions: psmux (visible)" or "ConPTY inside the Worker (hidden): psmux is not installed (winget install psmux)"). By default **the Hub refuses to create hidden sessions**: the New session button is disabled and says why and how to fix it (install psmux with `winget install psmux` and run `minion worker up --foreground` as yourself). You can allow hidden sessions for yourself in **Account > Sessions** (from your own signed-in browser session; an API key needs manage, and an administrator acting as you cannot change it); an administrator decides for everybody with the Hub setting `hidden_sessions` (`user-controlled`, the default, `allowed` or `disabled`). Sessions that already exist are not affected. A Windows Worker older than v0.10.0 does not report its session host; it counts as hidden (it may use ConPTY) until you update it.

`minion worker status` shows the run mode (foreground console or Windows service) and the session host with its reason. If the `MinionWorker` service is installed and running (or set to start at boot) while you start `minion worker up --foreground`, the Worker warns you: that service keeps serving hidden ConPTY sessions after you close the console. In an elevated PowerShell, stop and remove it with `sc.exe stop MinionWorker`, then `sc.exe delete MinionWorker` (a stopped service that starts at boot: `sc.exe delete MinionWorker`, or `sc.exe config MinionWorker start= demand` to keep it). Do not use `minion worker down` for this: once the service is stopped it stops the foreground Worker of your console instead. With psmux, a self-update is not refused for the sessions: psmux sessions survive the restart.

## Limits of the preview

- **Without psmux, sessions end with the Worker.** There is no server that outlives the Worker. Restarting or stopping the Worker ends every session, and the session list is empty afterwards. Sessions are not saved and not adopted again.
- **No replies through the pipe.** A message sent with `pipe_send` is delivered, but the Windows Worker extracts no agent reply from the screen (the reason reported is `no_evidence`). See [the pipe](../guides/pipe.md).
- **psmux checks and pastes in two steps.** With tmux the Worker checks that a pane still runs the program you reviewed and pastes in one step; psmux 3.4 runs that check and the paste as two requests, so a program that exits exactly then can leave the text to the shell that follows it. Session names that psmux cannot address literally (starting with `%`, `@`, `$` or `=`, or containing `:`, `.`, quotes, `#` or `;`) are not served.
- **The service runs as LocalSystem.** When you install the Worker as a Windows service, the sessions it creates run as that account, not as you, and it uses ConPTY (it does not see your psmux sessions). To run sessions as yourself, stop the service and run `minion worker up --foreground` in your own terminal.
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
minion worker down     # stop the running Worker (foreground or the Windows service)
minion worker reset    # after down: forget the Hub, the key and the share choice
```

`minion worker reset` forgets the Hub, the key and the share choice and refuses while the Worker runs: stop it first with `minion worker down`. It does not tell the Hub: remove the Worker on the Hub's Workers page too.

To diagnose a problem, run the Worker with `--debug` (or `MINION_DEBUG=1`). To let its owner or an administrator switch debug logging on and read its recent log from the Hub's Worker page, start it with `--allow-debug-remote` (or `minion worker config set allow_debug_remote true` and restart it); it is off by default. See [troubleshooting](../guides/troubleshooting.md#worker-debug---debug-and-minion_debug).
