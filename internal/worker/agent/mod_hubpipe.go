package agent

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Module name and start order of the Hub-mode pipe links (Story 7.4). It
// starts after the op server (210), next to which it serves every Hub link.
const (
	// ModuleHubPipe is the name of the module.
	ModuleHubPipe = "hubpipe"
	// OrderHubPipe is its start order.
	OrderHubPipe = 220
)

func init() { RegisterModule(ModuleHubPipe, OrderHubPipe, startHubPipe) }

// hubPipeDeps are the seams of the module that tests replace.
type hubPipeDeps struct {
	clock transport.Clock
}

func startHubPipe(ctx context.Context, env *Env) (StopFunc, error) {
	return hubPipeDeps{}.start(ctx, env)
}

// start composes the two Worker-side halves of the Hub pipe that Story 7.4
// owns, as handlers of every Hub link ([Connector.OnLink]) and as services for
// the modules of the stories that use them:
//
//   - the [TurnEmitter]: turn extraction (Story 7.2) calls [TurnEmitter.Emit]
//     for every agent reply of a shared session; the emitter keeps the turn in
//     its outbox, opens the turn channel on every link (an ev stream that
//     starts with turns.open), sends the pending turns oldest first, and sends
//     again after the ack timeout and after every reconnect until the Hub
//     answers turn.ack. It asks the share selection ([Policy]) before every
//     send, so an unshared session never reaches the Hub;
//   - the [CallClient]: a command on this Worker host that acts on another
//     Worker's session (`mn pipe_send` in a session, MCP on a Worker host in
//     Hub mode) forwards it to the Hub on a call stream with
//     [CallClient.Do] / [CallClient.Stream]. It speaks for the Worker identity
//     of the connection; with no Hub connection it fails with worker_offline
//     and queues nothing.
//
// Both are harmless without a Hub: no link ever arrives. The turn outbox is
// bounded ([DefaultOutboxLimit]) and lives in the Worker database
// ([StoreOutbox], Story 7.3); a Solo caller must not Emit.
func (d hubPipeDeps) start(_ context.Context, env *Env) (StopFunc, error) {
	s := env.Settings()
	log := s.logger()
	c, err := Require[*Connector](env)
	if err != nil {
		return nil, err
	}
	policy, err := Require[*Policy](env)
	if err != nil {
		return nil, err
	}
	// The outbox is durable when the Worker has its database (Story 7.3): a
	// restart between a reply and the Hub's turn.ack loses nothing.
	var outbox TurnOutbox
	if st, ok := lookupService[*store.Store](env.rt); ok && st != nil {
		outbox = NewStoreOutbox(st, DefaultOutboxLimit)
	}
	em, err := NewTurnEmitter(TurnConfig{Shares: policy, Outbox: outbox, Clock: d.clock, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("turn emitter: %w", err)
	}
	cc := NewCallClient(CallConfig{})
	if err := Provide(env, em); err != nil {
		return nil, err
	}
	if err := Provide(env, cc); err != nil {
		return nil, err
	}
	c.OnLink("turns", em.Serve)
	c.OnLink("hub-call", cc.Serve)
	log.Info("hub pipe links registered (turn channel and call client)")
	return nil, nil
}
