package api

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// WorkerRouter is what the Worker op handlers need; *workers.Router
// implements it.
type WorkerRouter interface {
	Call(ctx context.Context, req workers.CallRequest) (any, error)
	Stream(ctx context.Context, req workers.CallRequest, emit func(any) error) error
}

// RegisterWorkerOps registers the 11 core Worker ops (ops.CoreWorkerOpNames)
// in r, each routed to its Worker through rt. The dispatcher has already
// authenticated the caller, validated the arguments and decided the accepted
// action set on the Worker or session the arguments name; the router decides
// again and computes the effective actions it sends to the Worker (AD-12).
//
// Other Worker ops (share_get and share_set, Story 4.15) register through
// [RegisterWorkerOp] with the same router.
func RegisterWorkerOps(r *Registry, rt WorkerRouter) error {
	for _, name := range ops.CoreWorkerOpNames {
		if err := RegisterWorkerOp(r, name, rt); err != nil {
			return err
		}
	}
	return nil
}

// RegisterWorkerOp registers one Worker op of r's catalog: a streaming op as a
// stream handler, any other as a plain handler.
//
// The Hub routes to the resource the dispatcher decided the call on (Call.
// Resource): the Worker or session named by a Target or worker_id member of
// the arguments, or by the worker_id of the request (query or body) for an op
// whose arguments name no Worker. Anything else answers invalid_argument: a
// Worker op cannot run "somewhere".
func RegisterWorkerOp(r *Registry, name string, rt WorkerRouter) error {
	op, ok := r.catalog.Lookup(name)
	if !ok {
		return fmt.Errorf("api: %q is not an operation of the catalog", name)
	}
	if op.Scope != ops.ScopeWorker {
		return fmt.Errorf("api: %s is not a Worker operation", name)
	}
	request := func(c *Call) (workers.CallRequest, error) {
		// The dispatcher decided the call on c.Resource, from the Target or
		// worker_id of the arguments or, for an op whose arguments name no
		// Worker (sessions_list, session_create), from the worker_id of the
		// request. The router routes to exactly that resource.
		var workerID, session string
		switch res := c.Resource.(type) {
		case authz.SessionResource:
			workerID, session = res.WorkerID, res.Name
		case authz.WorkerResource:
			workerID = res.ID
		}
		if workerID == "" {
			return workers.CallRequest{}, invalidArg("worker_id", "the Worker that runs this operation must be named by worker_id")
		}
		return workers.CallRequest{
			Principal: c.Principal, Op: op.Name, WorkerID: workerID, Session: session, Args: c.Args,
		}, nil
	}
	if op.Stream {
		return r.HandleStream(name, func(ctx context.Context, c *Call, emit func(any) error) error {
			req, err := request(c)
			if err != nil {
				return err
			}
			return rt.Stream(ctx, req, emit)
		})
	}
	return r.Handle(name, func(ctx context.Context, c *Call) (any, error) {
		req, err := request(c)
		if err != nil {
			return nil, err
		}
		return rt.Call(ctx, req)
	})
}
