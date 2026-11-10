package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Trust decides whether a model may read the pipe turns of a peer (R29,
// AD-22). The peer is the session a pipe call addresses. A turn from a peer
// that is not trusted is never handed to the model; the tool result carries a
// notice instead.
//
// The Solo backend uses [TrustAll]: the OS user who runs `minion mcp` owns the
// Worker and every session on it. A Hub backend supplies its own policy.
type Trust interface {
	Trusted(ctx context.Context, peer ops.Target) bool
}

// TrustFunc adapts a function to [Trust].
type TrustFunc func(ctx context.Context, peer ops.Target) bool

// Trusted calls f.
func (f TrustFunc) Trusted(ctx context.Context, peer ops.Target) bool { return f(ctx, peer) }

// TrustAll trusts every peer. It is the policy of Solo mode, where the local
// owner's own sessions are the only peers.
var TrustAll Trust = TrustFunc(func(context.Context, ops.Target) bool { return true })

// TrustNone trusts no peer. It is the policy of a Server built without a
// [Trust], so that forgetting to configure one fails closed.
var TrustNone Trust = TrustFunc(func(context.Context, ops.Target) bool { return false })

// pipeTurns is the structured result of pipe_read and pipe_wait on MCP.
type pipeTurns struct {
	Turns []ops.PipeTurn `json:"turns"`
	// Withheld counts the turns that were not handed to the model: those the
	// backend (a Hub) withheld and those this server's Trust withheld.
	Withheld int `json:"withheld,omitempty"`
	// Reason is "untrusted_peer" when turns were withheld.
	Reason string `json:"reason,omitempty"`
	// Notice says why turns were withheld. It never carries their content.
	Notice string `json:"notice,omitempty"`
}

// maxNoticeBytes bounds a notice a backend sends, so that a notice stays a
// short note and not a channel for content.
const maxNoticeBytes = 512

// filterTurns keeps the turns when the peer is trusted and withholds all of
// them with a notice when it is not.
func filterTurns(trusted bool, peer ops.Target, turns []ops.PipeTurn) pipeTurns {
	out := pipeTurns{Turns: []ops.PipeTurn{}}
	if trusted {
		out.Turns = append(out.Turns, turns...)
		return out
	}
	if len(turns) == 0 {
		return out
	}
	out.Withheld = len(turns)
	out.Reason = string(proto.CodeUntrustedPeer)
	out.Notice = fmt.Sprintf("%d pipe turn(s) of session %q were withheld from the model because the peer is not trusted. A person can read them in the web UI or with the minion command line.",
		len(turns), peer.SessionName)
	return out
}

// shapeRead is the result of pipe_read for the model: the backend's turns
// filtered by this server's Trust, with what the backend already withheld
// carried over (the count, the reason and the notice a Hub sends). Turns the
// backend withheld never reach this server, so there is nothing of them to
// leak; only the metadata is kept, so that the model learns that something
// was withheld and why.
func shapeRead(trusted bool, peer ops.Target, r ops.PipeReadResult) pipeTurns {
	out := filterTurns(trusted, peer, r.Turns)
	if r.Withheld <= 0 {
		return out
	}
	out.Withheld += r.Withheld
	if out.Reason == "" {
		out.Reason = string(proto.CodeUntrustedPeer)
		if r.Reason != "" {
			out.Reason = r.Reason
		}
	}
	notice := r.Notice
	if len(notice) > maxNoticeBytes {
		notice = strings.ToValidUTF8(notice[:maxNoticeBytes], "") + "..."
	}
	switch {
	case notice == "":
		if out.Notice == "" {
			out.Notice = fmt.Sprintf("%d pipe turn(s) of session %q were withheld from the model because the peer is not trusted. A person can read them in the web UI or with the minion command line.",
				r.Withheld, peer.SessionName)
		}
	case out.Notice == "":
		out.Notice = notice
	default:
		out.Notice = notice + " " + out.Notice
	}
	return out
}
