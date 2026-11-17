package workers

// Hidden sessions (v0.10.0). A Worker reports in its hello whether a new
// session would be hidden from the person (transport.WorkerFacts: Windows
// ConPTY inside the Worker, or a tmux/psmux server that is not the person's
// default one). The Hub setting hidden_sessions and the caller's own
// preference decide whether session_create may make one; the Router refuses
// it before anything is sent, on the facts of the connection that would get
// it. A Worker that does not report its session host (older than v0.10.0)
// counts as hidden on Windows (it may host ConPTY) and as visible elsewhere.

import (
	"context"

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

// newSessionsHidden reports whether a new session on a Worker with these
// facts would be hidden. ConPTY is always hidden, whatever the flag says. A
// Worker older than v0.10.0 does not report its session host (legacy): on
// Windows it may host its sessions in ConPTY, so it counts as hidden; on
// Linux and macOS it uses tmux and is not refused.
func newSessionsHidden(f transport.WorkerFacts) (hidden, legacy bool) {
	switch {
	case f.SessionHost == transport.SessionHostConPTY:
		return true, false
	case f.SessionHost != "":
		return f.SessionsHidden, false
	case f.OS == "windows":
		return true, true
	}
	return false, false
}

// HiddenSessionRefusal is the message of a refused session_create on a Worker
// whose new sessions would be hidden, or "" when the policy allows it or the
// Worker's sessions are visible. It names the Worker, why its sessions are
// hidden and the exact fix.
func HiddenSessionRefusal(workerName string, f transport.WorkerFacts, pol HiddenSessionsPolicy) string {
	hidden, legacy := newSessionsHidden(f)
	if !hidden || pol.Allows() {
		return ""
	}
	if workerName == "" {
		workerName = "this Worker"
	}
	var msg string
	if legacy {
		msg = "New sessions on " + workerName + " may be hidden: it is a Windows Worker older than v0.10.0, which does not report whether its sessions are hidden (it may host them in ConPTY inside the Worker). " +
			"Update the Worker to v0.10.0 or later (from its Worker page in the Hub, or `minion update` on that machine)"
	} else {
		reason := f.SessionHostReason
		if reason == "" {
			reason = f.SessionHost
		}
		var fix string
		switch {
		case f.SessionHostReasonCode == transport.HostReasonConPTYService:
			fix = "Run the Worker as yourself instead of as a Windows service: `minion worker up --foreground`, with psmux installed (winget install psmux)"
		case f.SessionHost == transport.SessionHostConPTY:
			fix = "Install psmux (winget install psmux) and restart the Worker with `minion worker up --foreground`"
		case f.SessionHostReasonCode == transport.HostReasonHiddenSetting || f.SessionHostReasonCode == transport.HostReasonContainerTmux:
			fix = "Serve your own tmux server: mount your tmux socket into the Worker's container and set sessions_visible to yes (docs/install/worker-docker.md)"
		case f.SessionHost == transport.SessionHostTmux:
			fix = "Run the Worker as yourself on your default tmux server: `minion worker up --foreground` without a tmux_socket setting"
		default:
			fix = "Run the Worker as yourself: `minion worker up --foreground`"
		}
		msg = "New sessions on " + workerName + " would be hidden (" + reason + "). " + fix
	}
	if pol.Hub == config.HiddenSessionsDisabled {
		return msg + ". Hidden sessions are disabled on this Hub for everybody (Hub setting hidden_sessions)."
	}
	return msg + ", or allow hidden sessions in your settings (Account > Sessions)."
}

// hiddenSessionCheck returns the check of a session_create that refuses a
// hidden session the caller's policy does not allow, or nil for any other op.
// The policy and the Worker's name are read here; the check itself runs on
// the facts of the connection that gets the op (Backend.OpenOp), so a Worker
// that reconnects with other facts in between is judged on the new ones. A
// failed policy lookup refuses (fail closed).
func (rt *Router) hiddenSessionCheck(ctx context.Context, p authz.Principal, op, workerID string) (func(Online) error, error) {
	if rt.hidden == nil || op != ops.OpSessionCreate {
		return nil, nil
	}
	pol, err := rt.hidden(ctx, p)
	if err != nil {
		return nil, err
	}
	if pol.Allows() {
		return nil, nil
	}
	name := rt.workerName(ctx, workerID)
	return func(on Online) error {
		if msg := HiddenSessionRefusal(name, on.Facts, pol); msg != "" {
			return proto.NewError(proto.CodeForbidden, msg)
		}
		return nil
	}, nil
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
