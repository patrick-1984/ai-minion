// Package agentdetect classifies a pane by kind and state (AD-12). It is the
// only place in the Worker that produces the pair {kind, state}; every guard
// of the action contract reads this classification and names the state it
// reads.
//
// The closed sets are declared in internal/ops (PaneKind*, PaneState*):
//
//   - kind: "agent:<cli>", "shell", "other" or "unknown";
//   - state: "stable", "busy", "dialog" or "unknown".
//
// The package is split by concern:
//
//   - detect.go: Input, Result, the Detector, the kind decision and the
//     ProcessSource port;
//   - matchargs.go: which program a process runs, from its argv (shells,
//     interpreters and runners identify only the program they execute);
//   - procsource.go: the process table readers: /proc on Linux (no exec, with
//     the job-control facts), ps(1) on macOS and the BSDs;
//   - agents.go: the extensible table of known agent CLIs (claude, codex,
//     gemini, qwen, aider, opencode): how to recognise the process and the
//     screen markers of each;
//   - state.go: the state decision from a sanitised screen and the parsing of
//     dialog choices for the answer op;
//   - turns.go, turnanchor.go and turnrules.go: the turn extractor (Story 7.2).
//     It anchors the reply of an agent to a delivered pipe message by delivery
//     evidence from tmux (absolute row indexes and counters read in the paste's
//     own invocation, spine AM-5), never by searching the screen text, and
//     retires the delivery with ErrTurnUnavailable when that evidence is
//     invalidated. turnanchor.go holds the pure decisions, turnrules.go the
//     per-agent rules for the bottom chrome and the echo.
//
// Only a process on the pane's own terminal, in its foreground process group
// and not stopped, can make a pane an agent or a busy shell, the pane's root
// process included: a suspended or background agent, an agent on a nested pty
// and a wrapper whose argv outlived its agent leave the pane's input with the
// shell, and a message must never be typed into a shell (AD-12). The dialog
// choices of a menu driven by arrow keys are given only when the selected
// option can be read from the screen's styles.
//
// Classification never fails: when the evidence is missing or contradictory
// the result is kind "unknown" or "other" and state "unknown". The input
// screen must already be sanitised (AD-14); the package never reads raw
// terminal output.
//
// Owner: Stories 2.4 and 7.2.
package agentdetect
