package agent

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// SessionCounter counts the sessions that a stop or restart of this Worker
// would end (tray tickets v3, W2a). Only ConPTY sessions live inside the
// Worker process; tmux and psmux sessions outlive it, so for them the count is
// a known 0 without asking the host.
type SessionCounter struct {
	// Host lists the sessions; nil with EndsSessions means "cannot count".
	Host term.SessionHost
	// EndsSessions is true when a restart ends the host's sessions
	// (restartEndsSessions: Windows ConPTY).
	EndsSessions bool
}

// CountLost returns how many sessions a stop or restart would end. known is
// false when they cannot be counted (the listing failed, or there is no
// host): that is never reported as 0, so that no caller mistakes a listing
// error for "no session would end".
func (c SessionCounter) CountLost(ctx context.Context) (n int, known bool, err error) {
	if !c.EndsSessions {
		return 0, true, nil
	}
	if c.Host == nil {
		return 0, false, errNoSessionHost
	}
	list, err := c.Host.ListSessions(ctx)
	if err != nil {
		return 0, false, err
	}
	return len(list), true, nil
}

type sessionCountError string

func (e sessionCountError) Error() string { return string(e) }

const errNoSessionHost = sessionCountError("no session host to count the sessions")
