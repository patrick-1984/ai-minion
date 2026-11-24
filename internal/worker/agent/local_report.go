package agent

// F06: a write that a local caller (CLI, master session, MCP) makes on this
// Worker's own session through the local control socket is also reported to
// the Hub, which records it in its audit trail (proto.OpLocalAuditV1). The
// report holds the operation, the session, the outcome and the calling
// session; never message text or terminal content. It is best effort: the
// local write has already happened, and a Worker without a Hub connection or
// with an older Hub just does not report.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const localReportTimeout = 5 * time.Second

// reportLocalWrite sends the report of a finished local write, in the
// background. rawArgs are the request arguments, raw the result (nil on an
// error) and err the error of the call.
func (l localExecutor) reportLocalWrite(ctx context.Context, op string, rawArgs, raw json.RawMessage, err error, pane string) {
	o, ok := ops.Lookup(op)
	if !ok || o.Scope != ops.ScopeWorker || !o.Write || l.calls == nil {
		return
	}
	cc := l.calls()
	if c, ok := cc.(interface{ Connected() bool }); cc == nil || (ok && !c.Connected()) {
		return
	}
	rep := proto.LocalAuditArgs{Op: op, Outcome: "unknown"}
	var a struct {
		RequestID string      `json:"request_id"`
		Target    *ops.Target `json:"target"`
	}
	if json.Unmarshal(rawArgs, &a) == nil {
		rep.RequestID = a.RequestID
		if a.Target != nil {
			rep.Session = a.Target.SessionName
		}
	}
	switch {
	case err != nil:
		rep.Outcome = "error"
		if ae, ok := AsAPIError(err); ok {
			rep.Outcome = "refused"
			rep.Reason = string(ae.Code)
		}
	default:
		var res ops.WriteResult
		if json.Unmarshal(raw, &res) == nil {
			if res.Outcome != "" {
				rep.Outcome = string(res.Outcome)
			}
			rep.Reason = res.Reason
			if rep.Session == "" && res.Pane != nil {
				rep.Session = res.Pane.Session
			}
		}
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), localReportTimeout)
		defer cancel()
		rep.SourceSession = l.ex.sessionOfPane(rctx, pane)
		if derr := cc.Do(rctx, proto.OpLocalAuditV1, rep, nil); derr != nil {
			l.log.Debug("cannot report a local write to the Hub", "op", op, "error", derr.Error())
		}
	}()
}
