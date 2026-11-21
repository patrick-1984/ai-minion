package agent

import (
	"errors"
	"runtime"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// newManagedTmuxHost returns the tmux host for the server on socket with the
// managed host spec: the absolute tmux binary and, in the Android managed
// mode, -N and the fixed configuration on every call (the supervisor owns the
// server).
func newManagedTmuxHost(socket string, m config.ManagedHost) (term.SessionHost, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("a managed tmux host is not available on Windows")
	}
	return tmux.New(socket, tmux.OptionsFor(m)...)
}
