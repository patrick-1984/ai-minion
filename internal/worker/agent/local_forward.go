package agent

// SC-5: the Worker-side forward of session control (AD-22, AD-11).
//
// A local request (the CLI, `mn`, MCP on this host) whose target names another
// Worker cannot be served by this Worker's terminals. For the session-control
// ops ([forwardsToHub]) the local socket forwards it unchanged to the Hub on a
// call stream, like the pipe ops in Hub mode, and names the calling session in
// the call frame (source_session): the pane id the client sent ($TMUX_PANE) is
// mapped to its session through this Worker's own terminal host. The Hub
// decides with the session-control grant of that session; this Worker does not
// check one. A pane that cannot be mapped sends no source_session, and the Hub
// then answers as it does for any call without a grant.
//
// A target on this Worker (no Worker named, or this Worker's own id) stays on
// the local socket, unchanged. Without a Hub connection a forwarded request
// fails at once with worker_offline and nothing is queued.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// forwardsToHub reports whether the local request of op with args goes to the
// Hub: workers_list always (it is a Hub op), sessions_list when its worker_id
// names another Worker, and the read and write ops of session control when
// their target names another Worker. self is this Worker's id ("" when it has
// none, then every named Worker is another one).
func forwardsToHub(op string, args json.RawMessage, self string) bool {
	if op == ops.OpWorkersList {
		return true
	}
	if !ops.NamesWorker(op, args) {
		return false
	}
	var a struct {
		WorkerID string      `json:"worker_id"`
		Target   *ops.Target `json:"target"`
	}
	_ = json.Unmarshal(args, &a)
	id := a.WorkerID
	if op != ops.OpSessionsList && a.Target != nil {
		id = a.Target.WorkerID
	}
	return self == "" || id != self
}

// forwardSessionControl sends the local request rq to the Hub with the calling
// session of rq.Pane as its source_session.
func (l localExecutor) forwardSessionControl(ctx context.Context, rq Request, pane string) (json.RawMessage, error) {
	op, ok := ops.Lookup(rq.Op)
	if !ok {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "the Worker does not implement this operation")
	}
	if err := authorize(op.Actions, rq.Actions); err != nil {
		return nil, err
	}
	var cc PipeCaller
	if l.calls != nil {
		cc = l.calls()
	}
	if c, ok := cc.(interface{ Connected() bool }); cc == nil || (ok && !c.Connected()) {
		return nil, apiErr(proto.CodeWorkerOffline, "", "the request names another Worker, which only the Hub can reach, and this Worker is not connected to a Hub")
	}
	ctx = WithSourceSession(ctx, l.ex.sessionOfPane(ctx, pane))
	return forwardCall(ctx, cc, op, rq.Args)
}

// sessionOfPane returns the name of the session of this Worker's terminal host
// that holds the pane id pane, or "" when there is none, the host cannot be
// listed, or more than one session has that pane id (psmux numbers panes per
// session, v0.9.2): the caller's session is never guessed.
func (e *Executor) sessionOfPane(ctx context.Context, pane string) string {
	if pane == "" {
		return ""
	}
	all, err := e.host.ListSessions(ctx)
	if err != nil {
		return ""
	}
	found := ""
	for _, s := range all {
		for _, p := range s.Panes {
			if p.Ref.ID == pane {
				if found != "" && found != s.Name {
					return ""
				}
				found = s.Name
			}
		}
	}
	return found
}

// forwardCall sends a local request of op to the Hub as it is, with cc, and
// returns the Hub's answer. Arguments are not decoded: the request id of the
// caller stays the one the Hub records. A write whose call ended after it was
// sent has the outcome unknown (the Hub may have applied it): it is answered
// as that outcome, with a warning, and never repeated here (AD-15).
func forwardCall(ctx context.Context, cc PipeCaller, op ops.Op, raw json.RawMessage) (json.RawMessage, error) {
	var args any
	if len(raw) > 0 {
		args = raw
	}
	if op.Stream {
		err := cc.Stream(ctx, op.Name, args, func(el json.RawMessage) error { return EmitProgress(ctx, el) })
		if err != nil {
			return nil, forwardError(err)
		}
		return json.RawMessage(`{}`), nil
	}
	var res json.RawMessage
	if err := cc.Do(ctx, op.Name, args, &res); err != nil {
		var lost *CallLostError
		if op.Write && errors.As(err, &lost) {
			b, merr := json.Marshal(ops.WriteResult{
				Outcome: ops.OutcomeUnknown, Reason: proto.CodeWorkerOffline.String(),
				Warnings: []ops.Warning{{Code: "hub_call_lost", Message: "the connection to the Hub ended before it answered; the request may have been applied and is not sent again"}},
			})
			if merr != nil {
				return nil, internalErr("encode result", merr)
			}
			return b, nil
		}
		return nil, forwardError(err)
	}
	if len(res) == 0 {
		res = json.RawMessage(`null`)
	}
	return res, nil
}
