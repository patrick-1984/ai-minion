//go:build windows

package agent

import (
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/conpty"
)

// newSessionHost returns the ConPTY host: Windows has no tmux, the sessions
// live inside the Worker process (AD-13). The socket is not used.
func newSessionHost(string) (term.SessionHost, error) {
	return conpty.New(), nil
}
