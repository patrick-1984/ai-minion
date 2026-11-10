// Package tmuxtest starts tmux for tests on a private socket only and never
// touches the default tmux server (AD-25).
//
// A test calls New(t). New starts a tmux server as
//
//	tmux -S <t.TempDir()>/tmux.sock -f /dev/null
//
// with TMUX and TMUX_PANE removed from the child environment, and registers a
// t.Cleanup that runs kill-server for that one socket and nothing else. New
// fails the test when the computed socket equals the user's default tmux
// socket, skips the test when tmux is not installed (fails it when
// MINION_REQUIRE_TMUX=1, which CI sets), and the returned Server offers
// NewSession, SendKeys, Capture, WaitFor and Tmux (one command on that
// socket, for tests that resize, clear or respawn the pane under test).
//
// This package and the Worker tmux adapter (internal/worker/term/tmux) are the
// only places that exec tmux (AD-1). Tests must not run tmux themselves: the
// make check-tmux target rejects kill-server and direct tmux execs in test
// files outside this package.
//
// Owner: Story 1.5.
package tmuxtest
