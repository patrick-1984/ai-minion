# psmux as the Windows session host: spike result (v0.9.0)

Status: **evaluated, not enabled**. The Windows Worker keeps ConPTY in v0.9.0. This page records what the spike
proved, what blocks enabling psmux, and what a later release must do.

## Why

[psmux](https://github.com/psmux/psmux) (MIT, Rust) is a tmux-compatible server for Windows: each session runs in a
background server process, so sessions would survive a Worker restart or update, users could `psmux attach` to
them, and the Worker could reuse its tmux adapter instead of the separate ConPTY host (`internal/worker/term/conpty`,
whose sessions end with the Worker process).

## What the spike proved (Windows CI, psmux v3.4.0)

The `psmux` job of `.github/workflows/windows.yml` installs psmux v3.4.0 (pinned checksum) and runs
`internal/worker/term/tmux/psmux_windows_test.go` (build tag `psmux`) through the unchanged tmux adapter:

- version check, create a session, list it, set the `@minion_task` option, paste text, press Enter, snapshot and
  capture the screen: all pass;
- a session created by one Host is found by a new Host on the same socket (a restarted Worker): passes;
- `-S <path>` maps to a private namespace, and psmux reports that path as `#{socket_path}`; the default server is
  the label `default` in `%USERPROFILE%\.psmux`.

## What blocks enabling it (Codex gate rounds 1 and 2, verified)

1. **Pane ids are per session in psmux** (each session starts at `%1`); the adapter assumes ids unique per server.
   A snapshot or capture of a shared session can be routed to another, unshared session, and session creation can
   return the identity of another session. Sharing would leak. Fix needed: carry the session in every pane target
   (`=session:%id`) in the adapter, with a two-session regression test.
2. **Sharing filters go through more than one path** (`Policy.Selection` and `Policy.State`, the host observer):
   any psmux-specific restriction must be enforced in one place that every reader uses.
3. **Argument handling**: the adapter wraps a program as `env -- <program>` and falls back to `/bin/sh`; psmux runs
   the argv directly, so creating a session with a program needs Windows-native argv and shell discovery.
4. **Releases before 3.4.0 ignore `-S`** and would drive the user's default server: require ≥ 3.4.0.
5. **Key files**: psmux authenticates loopback TCP with key files in its data directory, created without a
   protected DACL; for a LocalSystem service this must be secured before use.
6. **Executable trust**: a service must run psmux only from a path no other account can change.
   `version.CheckTrustedExecutable` (added by this spike, tested on Windows CI) is the check to use.
7. `minion worker status` and the setup wizard count sessions with the `tmux` binary name; they must use the same
   resolved psmux executable and namespace.

## Next step

A story that fixes 1–7 behind an opt-in setting, with the psmux CI job extended by two-session isolation, key-file
ACL and service-identity tests, then a default switch once it has run in practice.
