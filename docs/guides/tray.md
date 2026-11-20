# Desktop tray

A small menu in the notification area (Windows), the top bar (macOS) or the panel (Ubuntu desktop) to control the Worker of your own user. It is for desktops only: a headless Linux host, a Raspberry Pi (armv7), the `minion` deb and rpm packages and the container image never contain it and have no GUI dependency. The tray is a separate program, `minion-tray`, in a separate download.

Every menu item runs a `minion` command (the table below), so anything the tray does you can also type. The tray shows nothing it did not read from `minion worker status --json`, and it needs no administrator rights.

## Install

Download the archive for your system from the release page (files `minion-tray-<version>-<os>-<arch>`; they are listed in the signed `checksums.txt` of the release, so the verification of the install guides covers them). The tray runs the `minion` that is next to it, else the one on `PATH`, so [install `minion`](../../README.md) first and check it with `minion worker status`. A Worker must be set up (menu item "Set up the Hub...", or `minion worker up --join -`) before Start does anything.

| System | Archive | Steps |
| --- | --- | --- |
| Windows 10/11 | `...-windows-amd64.zip`, `...-windows-arm64.zip` | Unzip, put `minion-tray.exe` next to `minion.exe` (or anywhere), double-click it. To start it at logon, put a shortcut in `shell:startup`. |
| Ubuntu desktop (GNOME) | `...-linux-amd64.tar.gz`, `...-linux-arm64.tar.gz` | Unpack, copy `minion-tray` to `~/.local/bin`, run it. It needs the `zenity` program for its dialogs (installed on Ubuntu desktop; otherwise `sudo apt install zenity`). GNOME shows no tray icons by itself: install the AppIndicator extension, `sudo apt install gnome-shell-extension-appindicator`, then log out and in and enable it in the Extensions app. KDE, Xfce and Cinnamon show the icon without it. |
| macOS 13 (Ventura) or newer, Intel and Apple Silicon | `...-darwin-universal.tar.gz` | Unpack, run `minion-tray`. This is the oldest macOS release the Go 1.27 toolchain supports, and the build has not been run on it yet. The binary is not notarized: if macOS refuses it, run `xattr -d com.apple.quarantine minion-tray` once. Add it to System Settings, General, Login Items to start it at login. |

`minion-tray --version` prints the version and exits. Quit the tray from its menu ("Quit tray"); that does not stop the Worker.

## The menu and its commands

| Menu item | Command |
| --- | --- |
| Status line, Hub address, Hub key words (also every 5 s and after each action) | `minion worker status --json` |
| Start | `minion worker start` |
| Stop (asks first when open sessions could end: a counted or unknown number of sessions, the ConPTY host of Windows, or a Windows Worker that did not answer) | `minion worker down` |
| Pause / Resume | `minion worker pause` / `minion worker resume` |
| Set up the Hub... (token pasted into a dialog, sent on standard input, never in an argument list; asks first when it could replace a Worker with open sessions) | `minion worker up --join -` |
| Shared sessions... (dialog: keep / share all / share none, and, when the rule is "select", a checklist of the shared sessions to uncheck; on Windows only keep / all / none) | `minion worker status --json` (the `share` section), then `minion worker config share all`, `share none`, or `share select -- <name>...` (nothing left checked is `share none`) |
| Allow new sessions (checkbox; a click asks to confirm the opposite of what the box shows; off blocks every new session on this machine, the Hub cannot lift it) | `minion worker config set allow_new_sessions on` / `off` |
| Refresh | `minion worker status --json` |
| Quit tray | none (closes the tray only) |

The Hub key line shows the six words of the Hub this Worker trusts: compare them with the ones your Hub shows. When the stored check says the key did not match, or the running Worker refuses the key the Hub now answers with, the line turns into `!! MISMATCH ... !!` and the icon red. Do not use that Hub until you know why.

A failed command shows its message in a dialog and the tray refreshes anyway. The checklist only narrows what is shared: it lists the sessions the rule "select" already shares, never the ones that are hidden. Every `minion` command has a 30 second limit. Details of each command: [Control a Worker from the command line](worker-control.md).

## Build it yourself

`make tray` builds `bin/minion-tray`. Linux and Windows are pure Go. macOS needs cgo (Xcode command line tools): `CGO_ENABLED=1 make tray`. Releases build it in the `tray` workflow, not with goreleaser, and `make lint` fails when a headless package imports a GUI library.

Back to the [README](../../README.md).
