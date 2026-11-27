package workers

// Session control on call streams (SC-3): a session that an admin gave a
// session-control grant lists, reads and types into its targets on other
// Workers through the call stream of its own Worker. The ops are served only
// to a caller with a grant ([CallServer.HandleGrantOnly]); for every other
// caller, a plain Worker identity or a call key, they stay forbidden. They go
// through the [Router] like REST: the same routing, the same authz.Decide, and
// an origin marked grant when the grant authorized the request.

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// SessionControlOps are the Worker ops a session-control grant reaches on call
// streams, besides workers_list.
var SessionControlOps = []string{
	ops.OpSessionsList, ops.OpSessionSnapshot, ops.OpSessionCapture,
	ops.OpSendText, ops.OpSendKeys, ops.OpSetTask, ops.OpAnswer,
	ops.OpSessionKeepSet,
}

// SessionControlRouter is what the session-control call ops need; *Router
// implements it.
type SessionControlRouter interface {
	Call(ctx context.Context, req CallRequest) (any, error)
	Stream(ctx context.Context, req CallRequest, emit func(any) error) error
	ListWorkers(ctx context.Context, p authz.Principal, all bool) ([]ops.WorkerInfo, error)
}

var _ SessionControlRouter = (*Router)(nil)

// RegisterSessionControlCalls makes the [SessionControlOps] and workers_list
// available on call streams for callers with a session-control grant, routed
// through rt.
func RegisterSessionControlCalls(cs *CallServer, rt SessionControlRouter) error {
	for _, name := range SessionControlOps {
		if err := cs.HandleGrantOnly(name, func(ctx context.Context, c *CallContext, emit func(any) error) (any, error) {
			req := CallRequest{Principal: c.Principal, Op: c.Op.Name, Args: c.Args, admit: c.admit, source: c.source()}
			if len(c.Principal.DirectGrants()) > 0 {
				// The grants behind the call: named by the audit event of a
				// write, and counted as used when the call is answered.
				req.granted = func(ids []string) { c.grantIDs = ids }
				// A116: the command shell grant that let a typed input skip
				// the shell_pane guard, for the same audit event.
				req.CommandShell = func(id string) { c.commandShell = id }
			}
			if t, ok := targetOfArgs(c.Args); ok {
				req.WorkerID, req.Session = t.WorkerID, t.SessionName
			} else {
				req.WorkerID = c.RequestWorkerID // sessions_list; the Router refuses ""
			}
			if c.Op.Stream {
				return nil, rt.Stream(ctx, req, emit)
			}
			if c.Op.Name == ops.OpSendText {
				if err := promptOnly(ctx, rt, c, &req); err != nil {
					return nil, err
				}
			}
			res, err := rt.Call(ctx, req)
			if err != nil || c.Op.Name != ops.OpSessionsList || len(c.Principal.DirectGrants()) == 0 {
				return res, err
			}
			// A direct grant of one session lets its holder list the target
			// Worker: the listing is cut down to the sessions it may read.
			return filterSessionsList(ctx, rt, c.Principal, req.WorkerID, res)
		}); err != nil {
			return err
		}
	}
	if err := registerFileAppCalls(cs, rt); err != nil {
		return err
	}
	// workers_list is a Hub op: every Worker, narrowed by authz to those the
	// grant (and the own-Worker rule) may read.
	return cs.HandleGrantOnly(ops.OpWorkersList, func(ctx context.Context, c *CallContext, _ func(any) error) (any, error) {
		list, err := rt.ListWorkers(ctx, c.Principal, true)
		if err != nil {
			return nil, err
		}
		return ops.WorkersListResult{Workers: list}, nil
	})
}

// FileAppCallOps are the file and app install ops an ordinary calling session
// reaches on call streams (v0.26.0): decided by the same rule as on the
// master lane, a direct grant or an applicable master acceptance that covers
// this caller, this target and this action.
var FileAppCallOps = []string{ops.OpFileList, ops.OpFilePull, ops.OpFilePush, ops.OpAppInstall, ops.OpAppInstallStatus}

// registerFileAppCalls makes the [FileAppCallOps] available on call streams
// for a calling session with a grant, routed through rt. A transfer that
// outlives the call is re-decided on the call's grants read again (its
// admission fence), so a revoke stops it.
func registerFileAppCalls(cs *CallServer, rt SessionControlRouter) error {
	for _, name := range FileAppCallOps {
		if err := cs.HandleGrantOnly(name, func(ctx context.Context, c *CallContext, _ func(any) error) (any, error) {
			req := CallRequest{Principal: c.Principal, Op: c.Op.Name, Args: c.Args, admit: c.admit, source: c.source()}
			if len(c.Principal.DirectGrants()) > 0 {
				req.granted = func(ids []string) { c.grantIDs = ids }
			}
			switch a := c.Args.(type) {
			case *ops.AppInstallArgs:
				req.WorkerID = a.WorkerID
				// The audit names the apps_install grant or acceptance row the
				// final admission decided on.
				req.appBasis = func(b authz.FileBasis) { c.grantIDs = appAuditIDs(b) }
			case *ops.AppInstallStatusArgs:
				req.WorkerID = a.WorkerID
			default:
				t, ok := targetOfArgs(c.Args)
				if !ok {
					return nil, proto.NewError(proto.CodeInternal, "unreadable file operation arguments")
				}
				req.WorkerID, req.Session = t.WorkerID, t.SessionName
			}
			if c.admit != nil && (c.Op.Name == ops.OpFilePull || c.Op.Name == ops.OpFilePush) {
				admit := c.admit
				req.fileRefresh = func(ctx context.Context) (authz.Principal, error) { return admit(ctx, Online{}) }
			}
			return rt.Call(ctx, req)
		}); err != nil {
			return err
		}
	}
	return nil
}

// MessageJudge says whether a direct grant gives a principal message on one
// session while it holds no operate there; *Router implements it.
type MessageJudge interface {
	HoldsMessageOnly(ctx context.Context, p authz.Principal, workerID, session string) (bool, error)
}

var _ MessageJudge = (*Router)(nil)

// promptOnly turns the send_text of a caller whose direct grant on the target
// session is a Message grant (message without operate) into a prompt: to an
// agent pane only, never forced, and always submitted: the Hub sets submit
// itself (D3), so a prompt is never left typed and unsent because the caller
// omitted the flag. Every other caller is left as it is, and so is a router
// that cannot judge (its decision then refuses).
func promptOnly(ctx context.Context, rt SessionControlRouter, c *CallContext, req *CallRequest) error {
	mj, ok := rt.(MessageJudge)
	if !ok || len(c.Principal.DirectGrants()) == 0 || req.Session == "" {
		return nil
	}
	only, err := mj.HoldsMessageOnly(ctx, c.Principal, req.WorkerID, req.Session)
	if err != nil || !only {
		return err
	}
	a, ok := c.Args.(*ops.SendTextArgs)
	if !ok || a == nil {
		return proto.NewError(proto.CodeInternal, "unreadable send_text arguments")
	}
	if a.Force {
		return proto.NewError(proto.CodeForbidden, "a Message grant sends prompts only")
	}
	prompt := *a
	prompt.AgentOnly, prompt.Submit = true, true
	req.Args, req.PromptOnly = prompt, true
	return nil
}

// SessionReader says whether a principal may read one session; *Router
// implements it.
type SessionReader interface {
	CanReadSession(ctx context.Context, p authz.Principal, workerID, session string) (bool, error)
}

// filterSessionsList keeps the sessions of a sessions_list result that p may
// read. A router that cannot judge single sessions, or a result that is not
// a session listing, answers nothing (fail closed).
func filterSessionsList(ctx context.Context, rt SessionControlRouter, p authz.Principal, workerID string, res any) (any, error) {
	sr, ok := rt.(SessionReader)
	out, isList := res.(ops.SessionsListResult)
	if !ok || !isList {
		return nil, proto.NewError(proto.CodeInternal, "unreadable session listing")
	}
	sessions := make([]ops.SessionInfo, 0, len(out.Sessions))
	for _, s := range out.Sessions {
		allowed, err := sr.CanReadSession(ctx, p, workerID, s.Name)
		if err != nil {
			return nil, err
		}
		if allowed {
			sessions = append(sessions, s)
		}
	}
	out.Sessions = sessions
	return out, nil
}
