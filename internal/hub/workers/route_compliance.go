package workers

import (
	"context"
	"errors"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// observe records a routed Worker call for compliance monitoring. The Router
// is the one place where REST (browser, CLI, MCP) and Worker session-control
// calls converge, so every terminal operation is recorded here once. A call
// that was refused before dispatch (c is nil: authorization, Minion control,
// hidden sessions) is not recorded. Pipe ops are recorded by the pipe broker.
// What it records is the request as submitted and the outcome the call
// returned; it infers nothing from retries. Content of a Worker that is not
// user-managed is left out (the action record stays).
func (rt *Router) observe(ctx context.Context, req CallRequest, c *call, res any, err error) {
	mon := rt.mon
	if mon.Mode() == compliance.Off || c == nil {
		return
	}
	switch req.Op {
	case ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait:
		return
	}
	outcome := "ok"
	if wr, ok := res.(ops.WriteResult); ok {
		outcome = string(wr.Outcome)
	}
	if err != nil {
		outcome = "error"
		var pe *proto.Error
		if errors.As(err, &pe) {
			outcome = string(pe.Code)
		}
	}
	attrs := append(PrincipalAttrs(req.Principal),
		compliance.KV{Key: "minion.op", Value: req.Op},
		compliance.KV{Key: "minion.outcome", Value: outcome},
		compliance.KV{Key: "minion.worker.id", Value: req.WorkerID})
	if req.Session != "" {
		attrs = append(attrs, compliance.KV{Key: "minion.session", Value: req.Session})
	}
	mon.Emit(compliance.Actions, compliance.EventAction, "", attrs...)
	if mon.Mode() < compliance.Output || !rt.contentAllowed(ctx, req) {
		return
	}
	if err == nil {
		if kind, text := outputText(res); kind != "" {
			mon.Emit(compliance.Output, compliance.EventOutput, text, append(attrs, compliance.KV{Key: "minion.output.kind", Value: kind})...)
		}
	}
	if kind, text := inputText(req.Args); kind != "" {
		mon.Emit(compliance.Full, compliance.EventInput, text, append(attrs, compliance.KV{Key: "minion.input.kind", Value: kind})...)
	}
}

func (rt *Router) contentAllowed(ctx context.Context, req CallRequest) bool {
	return rt.ContentAllowed(ctx, req.WorkerID)
}

// ContentAllowed reports whether content of the Worker's sessions may be
// recorded for compliance monitoring: it is user-managed (a failed lookup
// says no).
func (rt *Router) ContentAllowed(ctx context.Context, workerID string) bool {
	if rt.userMgd == nil {
		return true
	}
	ok, err := rt.userMgd(ctx, workerID)
	return err == nil && ok
}

// PrincipalAttrs names who made a call: the kind, the user (the act-as target
// for act-as, with the admin), the API key and the source Worker.
func PrincipalAttrs(p authz.Principal) []compliance.KV {
	a := []compliance.KV{{Key: "minion.principal.kind", Value: string(p.Kind)}}
	add := func(k, v string) {
		if v != "" {
			a = append(a, compliance.KV{Key: k, Value: v})
		}
	}
	add("minion.user.id", p.UserID)
	add("minion.acting_admin.id", p.ActorID)
	add("minion.api_key.id", p.KeyID)
	if p.Kind == authz.KindWorker {
		add("minion.source_worker.id", p.WorkerID)
	}
	return a
}

// outputText is the screen text a read returned.
func outputText(res any) (kind, text string) {
	switch r := res.(type) {
	case ops.SessionSnapshotResult:
		return "snapshot", strings.Join(r.Screen.Lines, "\n")
	case ops.SessionCaptureResult:
		return "capture", strings.Join(r.Screen.Lines, "\n")
	}
	return "", ""
}

// inputText is the content a write asked the Hub to send into a session. The
// arguments arrive as values or pointers.
func inputText(args any) (kind, text string) {
	switch a := args.(type) {
	case ops.SendTextArgs:
		return "text", a.Text
	case *ops.SendTextArgs:
		return inputText(*a)
	case ops.SendKeysArgs:
		return "keys", strings.Join(a.Keys, " ")
	case *ops.SendKeysArgs:
		return inputText(*a)
	case ops.AnswerArgs:
		return "answer", a.Choice
	case *ops.AnswerArgs:
		return inputText(*a)
	case ops.SetTaskArgs:
		return "task", a.Task
	case *ops.SetTaskArgs:
		return inputText(*a)
	case ops.SessionCreateArgs:
		// The directory and the program with its arguments: a command line
		// can carry a secret.
		return "session_create", strings.Join(append([]string{a.Cwd}, a.Argv...), "\n")
	case *ops.SessionCreateArgs:
		return inputText(*a)
	}
	return "", ""
}
