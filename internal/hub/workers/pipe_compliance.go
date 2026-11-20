package workers

import (
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The Pipe is the one place that records pipe traffic for compliance
// monitoring, for REST and Worker-call entry points alike (the Router skips
// the pipe ops it routes for the broker). Only calls the caller was
// authorised for are recorded. A turn is content only when the Worker of its
// session is user-managed. Received turns are output; the text of a sent
// message is input and is recorded only at the full level, also when a read
// returns it again.

func (p *Pipe) pipeAttrs(pr authz.Principal, op string, peer pipe.Peer, outcome string) []compliance.KV {
	return append(PrincipalAttrs(pr),
		compliance.KV{Key: "minion.op", Value: op},
		compliance.KV{Key: "minion.outcome", Value: outcome},
		compliance.KV{Key: "minion.worker.id", Value: peer.WorkerID},
		compliance.KV{Key: "minion.session", Value: peer.SessionName})
}

func (p *Pipe) contentOK(ctx context.Context, peer pipe.Peer) bool {
	return p.contentAllowed == nil || p.contentAllowed(ctx, peer.WorkerID)
}

func outcomeOf(wr ops.WriteResult, err error) string {
	if err == nil {
		return string(wr.Outcome)
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return string(pe.Code)
	}
	return "error"
}

// observeSend records an authorised pipe_send.
func (p *Pipe) observeSend(ctx context.Context, pr authz.Principal, peer pipe.Peer, args ops.PipeSendArgs, wr ops.WriteResult, err error) {
	mon := p.mon
	if mon.Mode() == compliance.Off {
		return
	}
	attrs := p.pipeAttrs(pr, ops.OpPipeSend, peer, outcomeOf(wr, err))
	mon.Emit(compliance.Actions, compliance.EventAction, "", attrs...)
	if mon.Mode() == compliance.Full && p.contentOK(ctx, peer) {
		mon.Emit(compliance.Full, compliance.EventInput, args.Text, append(attrs, compliance.KV{Key: "minion.input.kind", Value: "pipe_send"})...)
	}
}

// observeTurns records the turns a read or a wait returned and, when asked,
// the action. withheld counts received turns that were not returned.
func (p *Pipe) observeTurns(ctx context.Context, pr authz.Principal, op string, peer pipe.Peer, turns []ops.PipeTurn, withheld int, errOut error) {
	mon := p.mon
	if mon.Mode() == compliance.Off {
		return
	}
	attrs := p.pipeAttrs(pr, op, peer, outcomeOf(ops.WriteResult{Outcome: "ok"}, errOut))
	mon.Emit(compliance.Actions, compliance.EventAction, "", append(attrs,
		compliance.KV{Key: "minion.turns.returned", Value: int64(len(turns))},
		compliance.KV{Key: "minion.turns.withheld", Value: int64(withheld)})...)
	if mon.Mode() < compliance.Output || !p.contentOK(ctx, peer) {
		return
	}
	for _, t := range turns {
		min := compliance.Output
		if t.Direction == ops.PipeSent {
			min = compliance.Full
		}
		mon.Emit(min, compliance.EventOutput, t.Text, append(attrs,
			compliance.KV{Key: "minion.output.kind", Value: "pipe_turn"},
			compliance.KV{Key: "minion.turn.id", Value: t.TurnID},
			compliance.KV{Key: "minion.turn.direction", Value: t.Direction})...)
	}
}
