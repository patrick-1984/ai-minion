//go:build windows

package agent

import (
	"fmt"
	"os/user"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/conpty"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// psmuxSpecPrefix marks a host spec that names the psmux executable.
const psmuxSpecPrefix = "psmux="

// newSessionHost returns the session host that sessionHostSpec chose: psmux
// (the user's default psmux namespace, v0.9.2) or ConPTY, whose sessions live
// inside the Worker process (AD-13).
func newSessionHost(spec string) (term.SessionHost, error) {
	if bin, ok := strings.CutPrefix(spec, psmuxSpecPrefix); ok {
		return tmux.NewPsmux(bin)
	}
	return conpty.New(), nil
}

// sessionHostSpec decides between psmux and ConPTY (session_host). psmux
// serves the psmux sessions of the user who runs the Worker, so it is used
// only when the Worker runs as that user: a service runs as LocalSystem, which
// has no psmux sessions of its own, and must not run a program from a
// user-writable directory. auto falls back to ConPTY with the reason in the log
// and in the info the Worker reports to the Hub (ConPTY sessions are hidden);
// an explicit psmux that cannot be used is an error.
func sessionHostSpec(s Settings) (string, SessionHostInfo, error) {
	mode := config.SessionHostAuto
	if s.Resolver != nil {
		r, err := s.Resolver.Resolve(config.KeySessionHost)
		if err != nil {
			return "", SessionHostInfo{}, err // an invalid value never falls back to auto
		}
		if r.Value != "" {
			mode = r.Value
		}
	}
	log := s.logger()
	facts := windowsHostFacts{}
	if u, err := user.Current(); err == nil {
		facts.User = u.Username
	}
	if mode == config.SessionHostConPTY {
		facts.ConPTYReason = "session_host is set to conpty"
		info := decideWindowsHost(facts)
		log.Info("sessions are hosted by ConPTY inside the Worker (hidden)", "reason", facts.ConPTYReason)
		return "conpty", info, nil
	}
	reason := ""
	bin := tmux.FindPsmux()
	switch {
	case underServiceManager():
		reason = "the Worker runs as a Windows service (LocalSystem); psmux sessions are served by a Worker that runs as you (minion worker up --foreground)"
	case bin == "":
		reason = "psmux is not installed (winget install psmux)"
	default:
		if _, err := tmux.NewPsmux(bin); err != nil {
			reason = err.Error()
		}
	}
	if reason == "" {
		facts.Psmux = true
		log.Info("sessions are hosted by psmux (visible)", "psmux", bin)
		return psmuxSpecPrefix + bin, decideWindowsHost(facts), nil
	}
	if mode == config.SessionHostPsmux {
		return "", SessionHostInfo{}, fmt.Errorf("session_host is psmux, but %s", reason)
	}
	facts.ConPTYReason = reason
	log.Info("sessions are hosted by ConPTY inside the Worker (hidden)", "reason", reason)
	return "conpty", decideWindowsHost(facts), nil
}
