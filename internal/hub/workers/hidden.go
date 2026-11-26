package workers

// Hidden sessions (v0.10.0). A Worker reports in its hello whether a new
// session would be hidden from the person (transport.WorkerFacts: Windows
// ConPTY inside the Worker, or a tmux/psmux server that is not the person's
// default one). The Hub setting hidden_sessions and the caller's own
// preference decide whether session_create may make one; the Router refuses
// it before anything is sent, on the facts of the connection that would get
// it. A Worker that does not report its session host (older than v0.10.0)
// counts as hidden on Windows (it may host ConPTY) and as visible on Linux and
// macOS. A Worker older than v0.9.0 does not report its OS either, and nothing
// in its hello tells Windows from Linux (same ops, features and build-less
// hello): its host is unknown, which counts as hidden under disabled and as
// visible otherwise, so that a Hub-only upgrade does not lock out every old
// Linux Worker.

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// HiddenSessionsPolicy is the policy that applies to one caller on one
// Worker.
type HiddenSessionsPolicy struct {
	// Hub is the Hub setting hidden_sessions (config.HiddenSessions*).
	Hub string
	// UserAllows is the caller's own account-wide preference (the API key
	// owner's for a key); false when there is no user, such as a Worker
	// identity. It is the default for a Worker without a choice of its own.
	UserAllows bool
	// Worker is the Worker's own choice (v0.23.0, set by its owner or an
	// administrator); nil when it has none and UserAllows decides.
	Worker *bool
}

// Allows reports whether the caller may create a hidden session: allowed
// always, disabled never, user-controlled (and any other value, which the
// policy function never passes) by the Worker's own choice, or without one by
// the user's account-wide preference.
func (p HiddenSessionsPolicy) Allows() bool {
	switch p.Hub {
	case config.HiddenSessionsAllowed:
		return true
	case config.HiddenSessionsDisabled:
		return false
	}
	if p.Worker != nil {
		return *p.Worker
	}
	return p.UserAllows
}

// HiddenSessionsPolicyFunc returns the policy for a principal, without the
// Worker's own choice (see [WorkerHiddenSessionsSource]).
type HiddenSessionsPolicyFunc func(ctx context.Context, p authz.Principal) (HiddenSessionsPolicy, error)

// WorkerHiddenSessionsSource reads the Workers' own hidden-sessions choices
// (v0.23.0); *store.Store implements it.
type WorkerHiddenSessionsSource interface {
	// WorkerHiddenSessions is one Worker's choice, nil without one.
	WorkerHiddenSessions(ctx context.Context, workerID string) (*bool, error)
	// ListWorkerHiddenSessions is the choice of every Worker that has one.
	ListWorkerHiddenSessions(ctx context.Context) (map[string]bool, error)
}

// hostVisibility is what the Hub knows about where a Worker's new sessions
// would live.
type hostVisibility int

const (
	hostVisible hostVisibility = iota
	hostHidden
	// hostLegacyWindows: a Windows Worker older than v0.10.0, which may host
	// its sessions in ConPTY.
	hostLegacyWindows
	// hostUnknown: a Worker older than v0.9.0 that reports neither its
	// session host nor its OS.
	hostUnknown
)

// newSessionsHost reports whether a new session on a Worker with these facts
// would be hidden. ConPTY is always hidden, whatever the flag says. A Worker
// older than v0.10.0 does not report its session host: on Windows it may host
// its sessions in ConPTY (hostLegacyWindows); on Linux and macOS it uses tmux
// (visible); without an OS (older than v0.9.0) it is unknown.
func newSessionsHost(f transport.WorkerFacts) hostVisibility {
	switch {
	case f.SessionHost == transport.SessionHostConPTY:
		return hostHidden
	case f.SessionHost != "":
		if f.SessionsHidden {
			return hostHidden
		}
		return hostVisible
	case f.OS == "windows":
		return hostLegacyWindows
	case f.OS == "":
		return hostUnknown
	}
	return hostVisible
}

// mayRefuse reports whether a policy that does not allow hidden sessions
// refuses a new session on a host of this visibility: hidden and legacy
// Windows always, unknown only when hidden sessions are disabled on the Hub.
func (v hostVisibility) mayRefuse(hub string) bool {
	switch v {
	case hostHidden, hostLegacyWindows:
		return true
	case hostUnknown:
		return hub == config.HiddenSessionsDisabled
	}
	return false
}

// HiddenSessionRefusal is the message of a refused session_create on a Worker
// whose new sessions would be hidden, or "" when the policy allows it or the
// Worker's sessions are visible. It names the Worker, why its sessions are
// hidden and the exact fix.
func HiddenSessionRefusal(workerName string, f transport.WorkerFacts, pol HiddenSessionsPolicy) string {
	vis := newSessionsHost(f)
	if pol.Allows() || !vis.mayRefuse(pol.Hub) {
		return ""
	}
	if workerName == "" {
		workerName = "this Worker"
	}
	const update = "Update the Worker to v0.10.0 or later (from its Worker page in the Hub, or `minion update` on that machine)"
	var msg string
	switch vis {
	case hostLegacyWindows:
		msg = "New sessions on " + workerName + " may be hidden: it is a Windows Worker older than v0.10.0, which does not report whether its sessions are hidden (it may host them in ConPTY inside the Worker). " + update
	case hostUnknown:
		msg = "New sessions on " + workerName + " may be hidden: it is a Worker older than v0.9.0, which reports neither its session host nor its operating system (session host unknown). " + update
	default:
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
		case f.SessionHostReasonCode == transport.HostReasonAndroidManaged:
			fix = "This Worker is the Minion Android app: its sessions live in the app's own tmux server and no other terminal can attach to them"
		case f.SessionHostReasonCode == transport.HostReasonHiddenSetting || f.SessionHostReasonCode == transport.HostReasonContainerTmux:
			fix = "Serve your own tmux server: mount your tmux socket into the Worker's container and set sessions_visible to yes (docs/install/worker-docker.md)"
		case f.SessionHost == transport.SessionHostTmux:
			fix = "Run the Worker as yourself on your default tmux server: `minion worker up --foreground` without a tmux_socket setting"
		default:
			fix = "Run the Worker as yourself: `minion worker up --foreground`"
		}
		msg = "New sessions on " + workerName + " would be hidden (" + reason + "). " + fix
	}
	switch {
	case pol.Hub == config.HiddenSessionsDisabled:
		return msg + ". Hidden sessions are disabled on this Hub for everybody (Hub setting hidden_sessions)."
	case pol.Worker != nil:
		return msg + ". Hidden sessions are refused on this Worker (its owner or an administrator can allow them on its Worker page, Hidden sessions)."
	}
	return msg + ", or allow hidden sessions on this Worker (its Worker page, Hidden sessions) or for all your Workers in your settings (Account > Sessions)."
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
	if pol.Hub == config.HiddenSessionsUserControlled && rt.workerHidden != nil {
		if pol.Worker, err = rt.workerHidden.WorkerHiddenSessions(ctx, workerID); err != nil {
			return nil, fmt.Errorf("workers: read the hidden-sessions choice of the Worker: %w", err)
		}
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
