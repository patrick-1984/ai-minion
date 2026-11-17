//go:build windows

package agent

import (
	"fmt"
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
// user-writable directory. auto falls back to ConPTY with the reason in the log;
// an explicit psmux that cannot be used is an error.
func sessionHostSpec(s Settings) (string, error) {
	mode := config.SessionHostAuto
	if s.Resolver != nil {
		r, err := s.Resolver.Resolve(config.KeySessionHost)
		if err != nil {
			return "", err // an invalid value never falls back to auto
		}
		if r.Value != "" {
			mode = r.Value
		}
	}
	log := s.logger()
	if mode == config.SessionHostConPTY {
		log.Debug("choosing the session host", "session_host", mode, "host", "conpty", "why", "set explicitly")
		return "conpty", nil
	}
	reason := ""
	bin, where := tmux.FindPsmuxWhere()
	service := underServiceManager()
	log.Debug("choosing the session host", "session_host", mode, "under_service_manager", service, "psmux", bin, "psmux_found", where)
	switch {
	case service:
		reason = "the Worker runs as a Windows service (LocalSystem); psmux sessions are served by a Worker that runs as you (minion worker up --foreground)"
	case bin == "":
		reason = "psmux is not installed (winget install psmux)"
	default:
		if _, err := tmux.NewPsmux(bin, tmux.WithLogger(log)); err != nil {
			reason = err.Error()
		}
	}
	if reason == "" {
		log.Info("sessions are hosted by psmux", "psmux", bin)
		return psmuxSpecPrefix + bin, nil
	}
	if mode == config.SessionHostPsmux {
		return "", fmt.Errorf("session_host is psmux, but %s", reason)
	}
	log.Info("sessions are hosted by ConPTY inside the Worker", "reason", reason)
	return "conpty", nil
}
