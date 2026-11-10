// Package tmux is the tmux adapter of the term.SessionHost port. It is the
// only production package that runs the tmux executable (AD-1, AD-13).
//
// The adapter talks to exactly one tmux server: the constructor requires an
// absolute socket path and every invocation passes -S <socket>. This file set
// holds the read path (Story 2.2): the constructor and exec helper (host.go),
// session listing (list.go), pane resolution (resolve.go) and screen reads
// (read.go). The write path (Story 2.5) reuses the exec helper: text and key
// delivery with the KEEP re-check (write.go), session creation (create.go) and
// session options (options.go). The adapter never kills or renames a session.
//
// Owner: Stories 2.2 and 2.5.
package tmux
