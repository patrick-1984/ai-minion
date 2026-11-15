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
)

// SessionControlOps are the Worker ops a session-control grant reaches on call
// streams, besides workers_list.
var SessionControlOps = []string{
	ops.OpSessionsList, ops.OpSessionSnapshot, ops.OpSessionCapture,
	ops.OpSendText, ops.OpSendKeys, ops.OpSetTask, ops.OpAnswer,
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
			req := CallRequest{Principal: c.Principal, Op: c.Op.Name, Args: c.Args}
			if t, ok := targetOfArgs(c.Args); ok {
				req.WorkerID, req.Session = t.WorkerID, t.SessionName
			} else {
				req.WorkerID = c.RequestWorkerID // sessions_list; the Router refuses ""
			}
			if c.Op.Stream {
				return nil, rt.Stream(ctx, req, emit)
			}
			return rt.Call(ctx, req)
		}); err != nil {
			return err
		}
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
