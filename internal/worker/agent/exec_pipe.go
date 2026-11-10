package agent

// Story 7.3: the pipe on the Worker (AD-23, AD-12, AD-15, AD-22).
//
// This file is the executor half of the pipe: the op pipe_send that the Hub
// routes to a Worker, and the delivery that every pipe message takes into a
// pane, whoever the broker is. The broker halves are in pipe_solo.go (the
// Worker is the broker, Solo mode) and in the Hub (Story 7.4, Hub mode); the
// extractor that reads the reply is in pipe_follow.go.
//
// # Who is the caller
//
// The origin of a call is not a field of a request: it is the entry point it
// came through, so no argument can claim it.
//
//   - A request that reaches [Executor.Execute] came from the Hub (the op
//     server, [OpServer]). The Hub is the broker in Hub mode: it has already
//     decided, limited, charged and stored the message before it routed the
//     write here. The handler below therefore only does the local write:
//     the guards of the story (an agent pane in the state stable; busy or
//     dialog is failed_not_sent with forceable, a shell pane needs operate),
//     the paste and Enter, and the result. It never calls the Solo broker, the
//     Worker's pipe store or the [CallClient]: that would store and charge the
//     message twice and open a call loop.
//   - A call from this host (the local socket, MCP on a Worker host) goes to
//     [PipeService.Local], which serves pipe_send, pipe_read and pipe_wait
//     (they are not ops of the executor, like OpLocalStatus). In Hub mode
//     (an enrollment exists) it forwards them to the Hub with the CallClient;
//     in Solo mode it is the broker. pipe_read and pipe_wait are never
//     executed for the Hub, so the Worker does not announce them.
//
// pipe_send is registered by [NewExecutor], with the first ops, so that the
// connector announces it in its first hello whatever order the modules start
// in (the connector reads [Executor.Ops] for every attempt).

import (
	"context"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// SoloWorkerID is the worker id of the peer of a Solo pipe. A Solo Worker has
// no Hub identity; the broker needs a non-empty id for the keys of its limits,
// budget and store.
const SoloWorkerID = "solo"

// pipeRoute says who is the broker of a delivered message, which decides
// where the reply of the agent goes.
type pipeRoute int

const (
	// routeHub: the Hub is the broker; the reply is emitted to the Hub with
	// the turn emitter and the Worker keeps no copy but the outbox.
	routeHub pipeRoute = iota
	// routeSolo: the Worker is the broker; the reply is committed to the
	// Worker's pipe store.
	routeSolo
)

// AttachPipe connects the pipe service, the owner of the reply extraction, to
// the executor. Without one a message is still delivered; no reply is
// extracted.
func (e *Executor) AttachPipe(s *PipeService) { e.pipe.Store(s) }

// pipeSend is the handler of pipe_send for a Hub-origin write: the local
// guarded delivery only (see the file comment).
func (e *Executor) pipeSend(ctx context.Context, rq Request) (any, error) {
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.PipeSendArgs) (ops.WriteArgs, ops.Target) { return a.WriteArgs, a.Target })
	if err != nil {
		return nil, err
	}
	// A Hub that predates the causal ids sends no message_id. The reply could
	// not be matched to the delivery and the turn emitter would refuse it, so
	// the message would be reported delivered with no reply ever coming back.
	// Refuse before anything is written, with an error that names the update; a
	// request that never reaches the pane is not cached and may be repeated once
	// the Hub is updated (spine AM-13).
	if args.MessageID <= 0 {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "the Hub must be updated: pipe_send carries no message_id, so the reply of the agent could not be matched to it; nothing was sent")
	}
	return e.write(ctx, op, args.WriteArgs, args.Target, true, func(ctx context.Context) (ops.WriteResult, error) {
		return e.pipeRun(ctx, rq, op, args, routeHub)
	})
}

// pipeRun delivers a pipe message to its pane inside a write frame: the guards
// of typed input (a shell pane, an open dialog and a busy agent refuse unless
// force is set), the paste of the message with delivery evidence, Enter, and
// the start of the reply extraction.
func (e *Executor) pipeRun(ctx context.Context, rq Request, op ops.Op, args ops.PipeSendArgs, route pipeRoute) (ops.WriteResult, error) {
	var warn *ops.Warning
	res, err := e.paneRun(ctx, rq, op, args.WriteArgs, args.Target, true, inputGuards,
		func(ctx context.Context, p *pane) (ops.Outcome, proto.Code, error) {
			o, r, w, err := e.pipeDeliver(ctx, p, args.Text, args.MessageID, route)
			warn = w
			return o, r, err
		})
	if err == nil && res.Outcome == ops.OutcomeDelivered && warn != nil {
		res.Warnings = append(res.Warnings, *warn)
	}
	return res, err
}

// pipeDeliver pastes the message and presses Enter, through the same guarded
// primitive as send_text with submit (Executor.pasteSubmit): one resolved pane,
// the expected identity checked before the paste and again before the Enter,
// the same partial and unknown outcomes. The message is pasted verbatim through
// the host's Anchorer, which reads the delivery evidence in the same tmux
// invocation (spine AM-5); a host without one pastes it as text and no reply can
// be extracted. Nothing is ever written twice: a failure after the paste is
// reported as an outcome and never retried here (AD-15).
func (e *Executor) pipeDeliver(ctx context.Context, p *pane, text string, messageID int64, route pipeRoute) (ops.Outcome, proto.Code, *ops.Warning, error) {
	anch, anchErr := agentdetect.AnchorerOf(e.host)
	var mark term.DeliveryMark
	o, r, err := e.pasteSubmit(ctx, p, "message", true, func(ctx context.Context) (perr error) {
		if anchErr != nil {
			return e.host.PasteText(ctx, p.ref, text, p.ref)
		}
		mark, perr = anch.PasteMarked(ctx, p.ref, text, p.ref)
		return perr
	})
	if err != nil || o != ops.OutcomeDelivered {
		return o, r, nil, err
	}
	return o, "", e.followDelivery(p, text, messageID, mark, anchErr, route), nil
}

// followDelivery ends the extraction of an earlier message of the session and
// starts that of this one. It returns the warning the sender gets when no
// reply can be extracted: the message is delivered all the same and the
// explanation is explicit, never a made-up turn.
func (e *Executor) followDelivery(p *pane, text string, messageID int64, mark term.DeliveryMark, anchErr error, route pipeRoute) *ops.Warning {
	svc := e.pipe.Load()
	session := p.info.Session
	if svc != nil {
		// An older delivery keeps the reply it has read; it cannot be followed
		// together with this one (the extractor follows one delivery).
		svc.supersede(session)
	}
	reason := ""
	if anchErr != nil {
		reason = agentdetect.ReasonNoEvidence
	} else {
		reason = agentdetect.MarkReason(mark)
	}
	if reason != "" {
		e.log.Info("a pipe message was delivered; no reply can be extracted", "session", session, "reason", reason)
		return &ops.Warning{
			Code:    "turn_unavailable",
			Message: "the message was delivered; no reply can be extracted from this pane (reason " + reason + ")",
		}
	}
	if svc == nil {
		e.log.Debug("a pipe message was delivered with no pipe service to read the reply", "session", session)
		return nil
	}
	svc.follow(followRequest{
		Session: session, Pane: p.ref, Agent: agentName(p.info.Kind),
		Text: text, Mark: mark, Route: route, MessageID: messageID,
	})
	return nil
}

// agentName returns the CLI name of an agent:* kind, "" for any other kind.
func agentName(kind string) string {
	if isAgentKind(kind) {
		return strings.TrimPrefix(kind, ops.PaneKindAgentPrefix)
	}
	return ""
}
