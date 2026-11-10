package agent

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Module name and start order of the Hub op server and event stream. It
// starts after the connector (order 200), whose links it serves.
const (
	// ModuleOpServer is the name of the op server and event stream module.
	ModuleOpServer = "opserver"
	// OrderOpServer is its start order.
	OrderOpServer = 210
)

func init() { RegisterModule(ModuleOpServer, OrderOpServer, startOpServer) }

// opServerDeps are the seams of the module that tests replace.
type opServerDeps struct {
	clock transport.Clock
}

func startOpServer(ctx context.Context, env *Env) (StopFunc, error) {
	return opServerDeps{}.start(ctx, env)
}

// start registers the op server and the event publisher as handlers of every
// Hub link. The event publisher ends with the link. The op server returns a
// stop hook that ends admission and waits for the writes in flight: a write is
// never cut off (AD-15), so it must finish before the modules that start
// earlier (the connector, the core with the store and the host) are stopped.
func (d opServerDeps) start(_ context.Context, env *Env) (StopFunc, error) {
	s := env.Settings()
	log := s.logger()
	c, err := Require[*Connector](env)
	if err != nil {
		return nil, err
	}
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}
	policy, err := Require[*Policy](env)
	if err != nil {
		return nil, err
	}
	host, err := Require[term.SessionHost](env)
	if err != nil {
		return nil, err
	}
	interval, err := ResolvePollInterval(s.Resolver)
	if err != nil {
		return nil, err
	}

	srv, err := NewOpServer(OpServerConfig{Executor: ex, Policy: policy, Logger: log})
	if err != nil {
		return nil, err
	}
	obs, err := NewHostObserver(HostObserverConfig{Host: host, Classifier: ex.classify, Policy: policy})
	if err != nil {
		return nil, err
	}
	pub, err := NewEventPublisher(EventConfig{Observer: obs, Interval: interval, Clock: d.clock, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("event stream: %w", err)
	}
	if err := Provide(env, srv); err != nil {
		return nil, err
	}
	if err := Provide(env, pub); err != nil {
		return nil, err
	}
	c.OnLink("op-server", srv.Serve)
	c.OnLink("events", pub.Serve)
	log.Info("hub op server and event stream registered", "poll_interval", interval.String())
	return srv.Stop, nil
}
