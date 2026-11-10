//go:build !windows

package agent

import (
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// newSessionHost returns the tmux host for the server on socket (AD-13). The
// socket is the absolute path that config resolved once.
func newSessionHost(socket string) (term.SessionHost, error) {
	return tmux.New(socket)
}
