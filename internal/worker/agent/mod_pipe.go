package agent

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Module name and start order of the pipe (Story 7.3). It starts right after
// the core module (100) and before the local socket and the connector (200),
// so that the local socket serves the pipe from its first request. The
// services of the Hub links (the call client and the turn emitter of the
// hubpipe module, order 220) start later: the pipe looks them up for every
// call ([runtimePipeHub]) instead of requiring them at start.
//
// pipe_send is announced to the Hub by the executor itself, which registers it
// when it is built, so the order of the modules does not matter for the
// announcement.
const (
	// ModulePipe is the name of the module.
	ModulePipe = "pipe"
	// OrderPipe is its start order.
	OrderPipe = 150
)

func init() { RegisterModule(ModulePipe, OrderPipe, startPipe) }

// pipeDeps are the seams of the module that tests replace.
type pipeDeps struct {
	poll   time.Duration
	settle int
}

func startPipe(ctx context.Context, env *Env) (StopFunc, error) {
	return pipeDeps{}.start(ctx, env)
}

func (d pipeDeps) start(ctx context.Context, env *Env) (StopFunc, error) {
	st, err := Require[*store.Store](env)
	if err != nil {
		return nil, err
	}
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}
	log := env.Settings().logger()
	svc, err := NewPipeService(PipeConfig{
		Executor: ex, Store: st, Hub: &runtimePipeHub{rt: env.rt, st: st}, Logger: log,
		Poll: d.poll, Settle: d.settle,
	})
	if err != nil {
		return nil, err
	}
	if err := Provide(env, svc); err != nil {
		return nil, err
	}
	pruneCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	pruned := make(chan struct{})
	go func() {
		defer close(pruned)
		svc.RunPrune(pruneCtx, 0)
	}()
	log.Info("pipe started (Solo broker over the Worker store; Hub mode forwards to the Hub)")
	return func(stopCtx context.Context) error {
		cancel()
		<-pruned
		return svc.Stop(stopCtx)
	}, nil
}

// runtimePipeHub is the [PipeHub] of a running Worker: the mode is the
// enrollment in the Worker database, and the Hub link services are looked up
// in the runtime for every call.
type runtimePipeHub struct {
	rt *Runtime
	st *store.Store
}

// Enrolled implements [PipeHub].
func (h *runtimePipeHub) Enrolled(ctx context.Context) (bool, error) {
	_, err := h.st.LoadEnrollment(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotEnrolled):
		return false, nil
	}
	return false, err
}

// Calls implements [PipeHub].
func (h *runtimePipeHub) Calls() PipeCaller {
	if cc, ok := lookupService[*CallClient](h.rt); ok && cc != nil {
		return cc
	}
	return nil
}

// Turns implements [PipeHub].
func (h *runtimePipeHub) Turns() TurnSink {
	if em, ok := lookupService[*TurnEmitter](h.rt); ok && em != nil {
		return em
	}
	return nil
}
