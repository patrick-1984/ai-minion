//go:build windows

package agent

import (
	"fmt"
	"os/user"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/transport"
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
		log.Debug("choosing the session host", "session_host", mode, "host", "conpty", "why", "set explicitly")
		facts.ConPTYCode = transport.HostReasonConPTYSetting
		info := decideWindowsHost(facts)
		log.Info("sessions are hosted by ConPTY inside the Worker (hidden)", "reason", info.localReason())
		return "conpty", info, nil
	}
	reason := ""
	bin, where := tmux.FindPsmuxWhere()
	service := underServiceManager()
	log.Debug("choosing the session host", "session_host", mode, "under_service_manager", service, "psmux", bin, "psmux_found", where)
	switch {
	case service:
		facts.ConPTYCode = transport.HostReasonConPTYService
		reason = transport.HostReasonText(facts.ConPTYCode)
	case bin == "":
		facts.ConPTYCode = transport.HostReasonConPTYNoPsmux
		reason = "psmux is not installed (winget install psmux)"
	default:
		if _, err := tmux.NewPsmux(bin, tmux.WithLogger(log)); err != nil {
			facts.ConPTYCode, facts.ConPTYDetail = transport.HostReasonConPTYPsmuxFailed, err.Error()
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
	info := decideWindowsHost(facts)
	log.Info("sessions are hosted by ConPTY inside the Worker (hidden)", "reason", info.localReason())
	return "conpty", info, nil
}
