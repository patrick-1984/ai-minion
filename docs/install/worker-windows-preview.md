# Run a Worker on Windows (preview)

Back to the [README](../../README.md).

The Windows Worker is a **preview**. **Use release v0.14.0 or later**: v0.13.1 and older install the Worker as a LocalSystem service, which is a way to SYSTEM (see [moving from the LocalSystem service](#move-from-the-localsystem-service)); the logon autostart described here exists only from v0.14.0. If you have v0.13.1 or older, do not run `minion worker up` as administrator: first remove the old service as described below. It works differently from the Linux and macOS Worker, and some things are missing. Read the limits before you rely on it.

## What it is

**With psmux (0.9.2 and later).** [psmux](https://github.com/psmux/psmux) is tmux for Windows. When psmux 3.4.0 or later is installed (the Worker always runs as you, `minion worker up` or `minion worker up --foreground`), the Worker serves **your psmux sessions**: the ones you start with `psmux new -s work` show up in the Hub, and the sessions the Worker creates are psmux sessions you can `psmux attach` to. They keep running when the Worker stops, restarts or updates. On the first start in a console without psmux, the Worker asks once whether to install it with `winget install psmux` (the answer is kept in `psmux_offer`; `minion worker config set psmux_offer ask` asks again). `minion worker config set session_host conpty` (or `psmux`) chooses explicitly; the default `auto` uses psmux when it is installed.

Windows terminals that are not psmux sessions (a plain PowerShell or cmd window) are not visible: run the work you want to reach from the Hub inside psmux.

**Without psmux.** The Worker creates sessions as ConPTY (pseudo console) programs that live **inside the Worker process**. The same operations work on them: list, snapshot, capture, send text and keys, set a task, create a session, the Hub pages and the MCP tools.

## Visible and hidden sessions

A session is **visible** when you can see and join it from your own terminal: psmux sessions in your default namespace show in `psmux ls` and you can `psmux attach` to them. A session is **hidden** when you cannot: a ConPTY session lives inside the Worker process (psmux is not installed, or `session_host` is `conpty`). The same holds on Linux and macOS: sessions on your default tmux server are visible, sessions on a private socket (`tmux_socket`), in the tmux server of root under a service manager, or on the tmux server inside the Minion image are hidden (a `minion` installed by hand inside a container you work in, such as a Coder workspace, serves your own tmux server there: visible). The Worker setting `sessions_visible` (`auto`, `yes`, `no`; Linux and macOS) overrides that guess, for example `yes` for a container that mounts your own tmux socket ([Docker](worker-docker.md)).

The Worker reports its session host and the reason to the Hub. The Worker page and the Workers list show it ("Sessions: psmux (visible)" or "ConPTY inside the Worker (hidden): psmux is not installed (winget install psmux)"). By default **the Hub refuses to create hidden sessions**: the New session button is disabled and says why and how to fix it (install psmux with `winget install psmux` and run `minion worker up --foreground` as yourself). You can allow hidden sessions for yourself in **Account > Sessions** (from your own signed-in browser session; an API key needs manage, and an administrator acting as you cannot change it); an administrator decides for everybody with the Hub setting `hidden_sessions` (`user-controlled`, the default, `allowed` or `disabled`). Sessions that already exist are not affected. A Windows Worker older than v0.10.0 does not report its session host; it counts as hidden (it may use ConPTY) until you update it.

`minion worker status` shows the run mode (foreground console, or Windows logon autostart for the Worker that `minion worker up` installs) and the session host with its reason. With psmux, a self-update is not refused for the sessions: psmux sessions survive the restart.

## Limits of the preview

- **Without psmux, sessions end with the Worker.** There is no server that outlives the Worker. Restarting or stopping the Worker ends every session, and the session list is empty afterwards. Sessions are not saved and not adopted again.
- **No replies through the pipe.** A message sent with `pipe_send` is delivered, but the Windows Worker extracts no agent reply from the screen (the reason reported is `no_evidence`). See [the pipe](../guides/pipe.md).
- **psmux checks and pastes in two steps.** With tmux the Worker checks that a pane still runs the program you reviewed and pastes in one step; psmux 3.4 runs that check and the paste as two requests, so a program that exits exactly then can leave the text to the shell that follows it. Session names that psmux cannot address literally (starting with `%`, `@`, `$` or `=`, or containing `:`, `.`, quotes, `#` or `;`) are not served.
- **There is no Windows service.** The Worker runs as you, with your rights, never as LocalSystem; see [the Worker at logon](#run-it-at-logon). Releases up to 0.13.1 installed a LocalSystem service; see [moving from the LocalSystem service](#move-from-the-localsystem-service).
- The Windows build has had less testing than the Linux build. The pairing handshake has not had an independent cryptographic review yet (see [security](../security.md)); that applies to every platform.

## Install

Install **v0.14.0 or later**. While `releases/latest` still points to v0.13.1 or older, the one-line installer below installs an affected release: use the zip of a v0.14.0+ release from the releases page instead (by hand, below), and check with `minion version`.

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

Add `--hub-fingerprint <52 characters>`, read on the Hub host with `minion hub identity --words`, so that the Worker pairs only with that Hub; without it the first-run wizard in a terminal asks for it ([pairing](../guides/pairing.md#give-the-worker-the-hub-fingerprint)). A Worker that is not paired yet shows a code like `MN-K7Q4-XY12` (on the console and in the log) and waits. In the Hub open **Workers**, choose **Add worker** and type the code. The Worker then connects by itself and is **pending**: its log says `this Worker is waiting for approval`. Approve it in the Hub (**Workers**, compare the fingerprint, **Approve**) or with `minion hub worker approve <fingerprint|id>`. The Worker runs until you press Ctrl-C. Details: [pairing and sharing](../guides/pairing.md).

Quickest: the join command of the Hub (**Workers**, **Add worker**, **Add a Worker in one step**), `minion.exe worker up --foreground --join mnj1-...`, gives the Worker the Hub address, the Hub fingerprint and a one-time invitation key in one string ([add a Worker in one step](../guides/pairing.md#add-a-worker-in-one-step)). Instead of the code, an invitation key from the Hub (**Invitations**) enrolls the Worker: add `--invite-key -` and paste the key on standard input (other forms: [the invitation key](../guides/pairing.md#enroll-with-an-invitation-key)). The Worker is pending until you approve it, as above. `minion worker pair --server <url>` still pairs without starting the Worker.

`--share-existing` has no tmux sessions to count on Windows; it decides what the Hub may see of the sessions you create.

## Run it at logon

`minion worker up` without `--foreground`, in a normal terminal (not "as administrator"), installs the Worker for you: it writes the value `MinionWorker` into your own Run key, `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, which Windows runs at each logon of yours ([Run and RunOnce registry keys](https://learn.microsoft.com/windows/win32/setupapi/run-and-runonce-registry-keys)), and starts the Worker now. No administrator is needed, nothing is installed for other users, and no Windows service is involved. The value runs `minion.exe worker launch` from the `minion.exe` you installed (with `--state-dir` when you use another state directory): a small supervisor that runs `minion worker run` without a console window (`CREATE_NO_WINDOW`, [process creation flags](https://learn.microsoft.com/windows/win32/procthread/process-creation-flags)), starts it again 5 seconds after a failure (doubling up to a minute), at once after a self-update, and ends when you stop the Worker. Its output goes to `worker-launch.log` in the state directory, a file only you and SYSTEM can read (it can show a pairing code). The Worker is started without the `MINION_*` environment variables of your terminal: it uses the settings saved by `minion worker up`. At logon a console window may flash for a moment before the supervisor leaves it.

The Worker uses the same state directory as `minion worker up --foreground`, so it keeps your pairing, and it serves your psmux sessions. `minion worker up` again applies new flags: it stops the running Worker and starts it with the saved settings. The command refuses to run in an elevated prompt, because the Worker would then run with administrator rights. `minion worker down` also ends the supervisor, even while it waits to restart a failed Worker. To stop starting it at logon, stop it and remove the value: `minion worker down`, then `reg delete HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v MinionWorker /f`.

## Move from the LocalSystem service

Do this **before** you upgrade or run any `minion worker up`, including with v0.13.1 or older (which would install the LocalSystem service again when run as administrator). Releases up to 0.13.1 installed the Worker as the Windows service `MinionWorker`, running as LocalSystem from the `minion.exe` in your profile. Any program running as you could replace that file and so run code as SYSTEM. This release no longer runs the Worker as a service: if that old service starts, it ends at once and says so. `minion worker status` warns while it exists, and `minion worker up` refuses (exit status 21) until it is gone. `minion worker up` also refuses while it cannot find out whether the service exists (the service manager denied the query). Remove it in a PowerShell started as administrator, with Windows' own tools (never run your `minion.exe` elevated):

```powershell
$ErrorActionPreference='Stop'
Set-Service MinionWorker -StartupType Disabled   # first: no recovery action can start it again
Stop-Service MinionWorker -Force
if ((Get-Service MinionWorker).Status -ne 'Stopped') { throw 'the service still runs' }
sc.exe delete MinionWorker
if (Get-Service MinionWorker -ErrorAction SilentlyContinue) { 'still listed: close the Services console' } else { 'removed' }
```

Then run `minion worker up` in your normal terminal. (Only on an account that is always administrator, with no separate elevated prompt, does `minion worker up` remove the old service itself, with the same steps; that runs the `minion.exe` of your profile with administrator rights, so prefer the Windows commands above. `minion worker up --foreground` only warns, it does not refuse.)

## Start, pause, block new sessions, choose sessions

`minion worker start` starts the Worker that `minion worker up` installed (the logon supervisor; it installs nothing and leaves a running Worker alone); `minion worker pause` and `resume` take the Worker off the Hub and back without ending your sessions; `minion worker config set allow_new_sessions off` blocks new sessions from every origin; `minion worker config share list` and `share ... --revision` back a session picker (on Windows the sessions live inside the Worker, so the list shows the saved selection only). `minion worker up --join -` reads the join token from standard input. See [Control a Worker from the command line](../guides/worker-control.md).

## Check and reset

```powershell
minion worker status
minion worker config share all
minion worker down     # stop the running Worker (foreground, or the one started at logon)
minion worker reset    # after down: forget the Hub, the key and the share choice
```

`minion worker reset` forgets the Hub, the key and the share choice and refuses while the Worker runs: stop it first with `minion worker down`. It does not tell the Hub: remove the Worker on the Hub's Workers page too.

To diagnose a problem, run the Worker with `--debug` (or `MINION_DEBUG=1`). To let its owner or an administrator switch debug logging on and read its recent log from the Hub's Worker page, start it with `--allow-debug-remote` (or `minion worker config set allow_debug_remote true` and restart it); it is off by default. See [troubleshooting](../guides/troubleshooting.md#worker-debug---debug-and-minion_debug).
