package api

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Story 7.4: the REST (and MCP-over-REST) face of the Hub pipe (AD-23,
// AD-22). In Hub mode the Hub is the broker, so pipe_send, pipe_read and
// pipe_wait are not forwarded blindly to the Worker like the other Worker
// ops: the Hub pipe service ([workers.Pipe]) checks the limits and the task
// budget, stores the message, routes the write to the Worker, keeps the turns
// the Workers emit and withholds the turns of untrusted sources from a model
// (an API key caller). The dispatcher has already authenticated the caller,
// resolved a human target, validated the arguments and decided
// message|operate (pipe_send) or read (pipe_read, pipe_wait) on the session.

// PipeService is what the pipe handlers need; *workers.Pipe implements it.
type PipeService interface {
	Send(ctx context.Context, p authz.Principal, args ops.PipeSendArgs) (ops.WriteResult, error)
	Read(ctx context.Context, p authz.Principal, args ops.PipeReadArgs) (workers.PipeReadView, error)
	Wait(ctx context.Context, p authz.Principal, args ops.PipeWaitArgs, emit func(ops.PipeTurn) error) error
}

var _ PipeService = (*workers.Pipe)(nil)

// RegisterPipeOps registers the handlers of the three pipe ops in r, served
// by svc. It replaces the plain routing of those ops that
// [RegisterWorkerOp] would register: register either, not both.
func RegisterPipeOps(r *Registry, svc PipeService) error {
	if svc == nil {
		return fmt.Errorf("api: the pipe handlers need a pipe service")
	}
	if err := r.Handle(ops.OpPipeSend, func(ctx context.Context, c *Call) (any, error) {
		return svc.Send(ctx, c.Principal, *c.Args.(*ops.PipeSendArgs))
	}); err != nil {
		return err
	}
	if err := r.Handle(ops.OpPipeRead, func(ctx context.Context, c *Call) (any, error) {
		return svc.Read(ctx, c.Principal, *c.Args.(*ops.PipeReadArgs))
	}); err != nil {
		return err
	}
	return r.HandleStream(ops.OpPipeWait, func(ctx context.Context, c *Call, emit func(any) error) error {
		return svc.Wait(ctx, c.Principal, *c.Args.(*ops.PipeWaitArgs), func(t ops.PipeTurn) error { return emit(t) })
	})
}

// RegisterWorkerOpsWithPipe is [RegisterWorkerOps] for a Hub that is the pipe
// broker: the eight non-pipe core Worker ops are routed to their Workers
// through rt, and the three pipe ops are served by svc.
func RegisterWorkerOpsWithPipe(r *Registry, rt WorkerRouter, svc PipeService) error {
	for _, name := range ops.CoreWorkerOpNames {
		switch name {
		case ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait:
			continue
		}
		if err := RegisterWorkerOp(r, name, rt); err != nil {
			return err
		}
	}
	return RegisterPipeOps(r, svc)
}
