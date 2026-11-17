//go:build !windows

package agent

import (
	"os"
	"os/user"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// newSessionHost returns the tmux host for the server on socket (AD-13). The
// socket is the absolute path that config resolved once.
func newSessionHost(socket string) (term.SessionHost, error) {
	return tmux.New(socket)
}

// sessionHostSpec is the tmux socket: other platforms always use tmux. The
// info says whether its sessions are visible to the person (decideTmuxHost).
func sessionHostSpec(s Settings) (string, SessionHostInfo, error) {
	def, _ := config.SystemPathEnv().TmuxSocket()
	f := tmuxHostFacts{Socket: s.TmuxSocket, DefaultSocket: def, Root: os.Getuid() == 0, ServiceManager: underServiceManager()}
	if u, err := user.Current(); err == nil {
		f.User = u.Username
	}
	return s.TmuxSocket, decideTmuxHost(f), nil
}
