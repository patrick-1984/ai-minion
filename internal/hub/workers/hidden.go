package workers

// Hidden sessions (v0.10.0). A Worker reports in its hello whether a new
// session would be hidden from the person (transport.WorkerFacts: Windows
// ConPTY inside the Worker, or a tmux/psmux server that is not the person's
// default one). The Hub setting hidden_sessions and the caller's own
// preference decide whether session_create may make one; the Router refuses
// it before anything is sent. A Worker that does not report its session host
// (older than v0.10.0) is never refused.

import (
	"context"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// HiddenSessionsPolicy is the policy that applies to one caller.
type HiddenSessionsPolicy struct {
	// Hub is the Hub setting hidden_sessions (config.HiddenSessions*).
	Hub string
	// UserAllows is the caller's own preference (the API key owner's for a
	// key); false when there is no user, such as a Worker identity.
	UserAllows bool
}

// Allows reports whether the caller may create a hidden session: allowed
// always, disabled never, user-controlled (and any other value, which the
// policy function never passes) by the user's own choice.
func (p HiddenSessionsPolicy) Allows() bool {
	switch p.Hub {
	case config.HiddenSessionsAllowed:
		return true
	case config.HiddenSessionsDisabled:
		return false
	}
	return p.UserAllows
}

// HiddenSessionsPolicyFunc returns the policy for a principal.
type HiddenSessionsPolicyFunc func(ctx context.Context, p authz.Principal) (HiddenSessionsPolicy, error)

// HiddenSessionRefusal is the message of a refused session_create on a Worker
// whose new sessions would be hidden, or "" when the policy allows it or the
// Worker does not say that its sessions are hidden. It names the Worker, why
// its sessions are hidden and the exact fix.
func HiddenSessionRefusal(workerName string, f transport.WorkerFacts, pol HiddenSessionsPolicy) string {
	if f.SessionHost == "" || !f.SessionsHidden || pol.Allows() {
		return ""
	}
	if workerName == "" {
		workerName = "this Worker"
	}
	reason := f.SessionHostReason
	if reason == "" {
		reason = f.SessionHost
	}
	var fix string
	switch f.SessionHost {
	case transport.SessionHostConPTY:
		if strings.Contains(reason, "service") {
			fix = "Run the Worker as yourself instead of as a Windows service: `minion worker up --foreground`, with psmux installed (winget install psmux)"
		} else {
			fix = "Install psmux (winget install psmux) and restart the Worker with `minion worker up --foreground`"
		}
	case transport.SessionHostTmux:
		fix = "Run the Worker as yourself on your default tmux server: `minion worker up --foreground` without a tmux_socket setting"
	default:
		fix = "Run the Worker as yourself: `minion worker up --foreground`"
	}
	msg := "New sessions on " + workerName + " would be hidden (" + reason + "). " + fix
	if pol.Hub == config.HiddenSessionsDisabled {
		return msg + ". Hidden sessions are disabled on this Hub for everybody (Hub setting hidden_sessions)."
	}
	return msg + ", or allow hidden sessions in your settings (Account > Sessions)."
}

// checkHiddenSession refuses a session_create that would make a hidden
// session the caller's policy does not allow. A Worker that is offline or
// does not report its session host is left to the call itself. A failed
// policy lookup refuses (fail closed).
func (rt *Router) checkHiddenSession(ctx context.Context, p authz.Principal, op, workerID string) error {
	if rt.hidden == nil || op != ops.OpSessionCreate {
		return nil
	}
	on, ok := rt.backend.Get(workerID)
	if !ok || on.Facts.SessionHost == "" || !on.Facts.SessionsHidden {
		return nil
	}
	pol, err := rt.hidden(ctx, p)
	if err != nil {
		return err
	}
	msg := HiddenSessionRefusal(rt.workerName(ctx, workerID), on.Facts, pol)
	if msg == "" {
		return nil
	}
	return proto.NewError(proto.CodeForbidden, msg)
}

// workerName is the display name of a Worker, "" when it cannot be read.
func (rt *Router) workerName(ctx context.Context, workerID string) string {
	recs, err := rt.dir.Workers(ctx)
	if err != nil {
		return ""
	}
	for _, w := range recs {
		if w.ID == workerID {
			return w.Name
		}
	}
	return ""
}
