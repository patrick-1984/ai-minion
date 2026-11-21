//go:build !windows

package agent

import (
	"os"
	"os/user"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wsvc "github.com/patrick-1984/ai-minion/internal/worker/service"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// newSessionHost returns the tmux host for the server on socket (AD-13). The
// socket is the absolute path that config resolved once.
func newSessionHost(socket string) (term.SessionHost, error) {
	return tmux.New(socket)
}

// sessionHostSpec is the tmux socket: other platforms always use tmux. The
// info says whether its sessions are visible to the person (decideTmuxHost,
// with the setting sessions_visible).
func sessionHostSpec(s Settings) (string, SessionHostInfo, error) {
	if s.Managed.Android {
		// The app's private server: kept as a tmux host, reported as hidden,
		// so that the Hub policy for hidden sessions applies.
		return s.TmuxSocket, newHostInfo(transport.SessionHostTmux, true, transport.HostReasonAndroidManaged, "socket "+s.TmuxSocket), nil
	}
	visible := config.SessionsVisibleAuto
	if s.Resolver != nil {
		r, err := s.Resolver.Resolve(config.KeySessionsVisible)
		if err != nil {
			return "", SessionHostInfo{}, err // an invalid value never falls back to auto
		}
		if r.Value != "" {
			visible = r.Value
		}
	}
	def, _ := config.SystemPathEnv().TmuxSocket()
	f := tmuxHostFacts{Socket: s.TmuxSocket, DefaultSocket: def, Root: os.Getuid() == 0, ServiceManager: underServiceManager(),
		Container:   wsvc.InContainer(wsvc.SystemEnv()),
		MinionImage: runsFromMinionImage(os.Getenv, executablePath()), Visible: visible}
	if u, err := user.Current(); err == nil {
		f.User = u.Username
	}
	return s.TmuxSocket, decideTmuxHost(f), nil
}

// executablePath is the path of the running binary, "" when unknown.
func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}
