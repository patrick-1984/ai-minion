// Package fakeagent is a scriptable fake agent for tests that run a program in
// a real tmux pane: a prompt "> ", the echo of what is typed, and a reply that
// the test writes, row by row, into a control file (Story 7.2 turn tests).
//
// A test binary runs the agent by calling Run first in its TestMain and starts
// the pane with Argv, so no program other than the test binary itself is
// needed. The modes are:
//
//   - line: the terminal stays in line mode, so the tty echoes the typed text
//     after the prompt, as most agents' input rows show it;
//   - bare: line mode without a prompt glyph, with a footer after each reply,
//     for an agent that echoes the message plainly (opencode's layout);
//   - bp: raw mode with bracketed paste enabled; the agent echoes the text
//     itself, keeps a pasted LF as a new row of the same message and submits
//     once, on Enter;
//   - dump and dumpbp: raw mode, without and with bracketed paste, that print
//     "ready" once it is on and append every received byte to a file, as
//     hex, to compare what a paste delivered.
//
// After a message is submitted the agent reads commands from the control file
// until "done" (see the command list in agent.go), so that the test decides
// when each reply row appears and can act on the pane between rows.
//
// The package is test support only: it never runs outside a test.
//
// Owner: Story 7.2.
package fakeagent
