//go:build windows

package agent

import (
	"os/exec"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/conpty"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// newSessionHost returns the session host of a Windows Worker. An empty socket
// means ConPTY: the sessions live inside the Worker process (AD-13). A socket
// means psmux, the tmux-compatible server for Windows, driven by the same tmux
// adapter as on Linux and macOS: its sessions belong to the psmux server and
// survive a Worker restart (windows_session_host, resolved in config).
func newSessionHost(socket string) (term.SessionHost, error) {
	if socket == "" {
		return conpty.New(), nil
	}
	bin, err := exec.LookPath("psmux")
	if err != nil {
		return nil, err
	}
	return tmux.New(socket, tmux.WithBinary(bin))
}
