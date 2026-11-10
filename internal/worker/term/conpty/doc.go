// Package conpty is the Windows ConPTY adapter of the SessionHost port. It is the only package that imports go-pty (AD-1, AD-13).
//
// Owner: Story 8.1.
//
// # Model
//
// Each session has exactly one pane (window 0, pane 0) and lives inside the
// Worker process: the Worker starts the program in a Windows pseudo console
// and keeps the master end. There is no server that outlives the Worker, so a
// Worker restart ends every session and sessions_list is empty afterwards.
// This is a documented limitation of the Windows preview (R98); sessions are
// not persisted and not re-adopted.
//
// # Layers
//
// The package is split so that everything except the pseudo console itself is
// platform neutral and unit-testable on any OS:
//
//   - vt.go is a minimal VT screen buffer (cursor, SGR, erase, insert and
//     delete, scroll regions, alternate screen, scrollback). It is fed with
//     the pseudo console output, answers the few queries ConPTY itself sends
//     (cursor position, device attributes) and renders to the one screen
//     format through term.BuildScreen, so every snapshot passes the sanitiser
//     (AD-14).
//   - keys.go maps tmux key names (the send_keys vocabulary) to the VT input a
//     program in the pseudo console expects.
//   - host.go holds the session table and implements term.SessionHost over two
//     small ports (proc and starter). It owns the KEEP re-check: before every
//     write the pane id, process id and command are compared with the expected
//     PaneRef.
//   - host_windows.go (build tag windows) is the only file that touches
//     go-pty: it implements the starter on top of a ConPTY and exports New.
//
// On other platforms the package compiles without New, so nothing here is
// reachable from a Unix Worker (the tmux adapter serves those).
//
// # Behaviour notes
//
//   - A session whose process has exited stays listed with its last screen, so
//     a short command such as "cmd /c echo hi" can still be read. A write to
//     it fails with term.ErrPaneChanged: the process the caller addressed is
//     gone. The host never kills or renames a session on its own initiative;
//     Close ends them all at Worker shutdown.
//   - PasteText delivers the text verbatim, the same contract as the tmux
//     adapter (term.CheckPasteText): TAB and LF reach the program unchanged
//     (LF is not turned into CR, which is Enter), the adapter adds no Enter,
//     and CR, ESC, NUL, DEL and the C1 range are refused before anything is sent
//     whoever calls the port. Intentional control input is SendKeys with a key
//     name. The text is wrapped in bracketed-paste markers only when the
//     program asked for them (DECSET 2004). Without them the bytes arrive
//     unwrapped, and a console program that reads a received LF as a
//     submission may act on it: LF is delivered as LF, not promised inert in
//     every program. Text and keys travel through the pseudo console's input
//     pipe, never on a command line.
//   - Zero-width (combining) characters are not kept by the screen buffer.
package conpty
