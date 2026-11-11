package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// pipeModuleOrder places the pipe module after the events module (55), whose
// ingest takes the turn channel, and the routing module (50), whose router
// carries the pipe_send write to the Worker.
const pipeModuleOrder = 60

// pipePruneInterval is the interval of the retention loop; zero selects the
// default of the pipe (one hour). A test shortens it.
var pipePruneInterval time.Duration

func init() {
	// The budget is resolved for every send, so a panel change applies at once.
	DeclareLiveSetting(config.KeyPipeBudgetPerDay)
	registerPipeModule(DefaultRegistry)
}

// registerPipeModule registers the Hub pipe module in r (Story 7.4, AD-23).
// The Hub is the pipe broker: this module builds the one [workers.Pipe] (the
// one broker over the one SQLite store) and composes every way into it, so that
// a default Hub serves
//
//   - REST pipe_send, pipe_read and pipe_wait (also the MCP and API-key
//     clients, which come through REST): the handlers are registered on the api
//     handler registry, instead of plain routing to a Worker (the route module
//     leaves the three ops to this one);
//   - the call streams of a Worker (`mn pipe_send` in a session on another
//     Worker, a Worker-bound API key on a Worker host): the call server
//     registers the three ops on the connect handler's call loop with
//     [workers.CallServer.Bind], next to worker_retire, which stays Story 4.7's;
//   - the turn channel and its turn.ack: the pipe serves the "ev" streams whose
//     first frame is turns.open through the event ingest ([workers.Pipe.RegisterTurns]),
//     next to the regular event streams;
//   - the 7 day retention: a prune loop that starts with the Hub and stops
//     (and is waited for) before the store closes.
//
// Nothing here reads a session: the one accept loop of a Worker session is the
// connect handler's stream host. A registration that is refused (an op or a
// first frame that something else already serves) fails the start of the Hub
// instead of silently replacing the other handler. Trust, the authorizer's
// policy and the task budget (setting pipe_budget_per_day) are read live.
func registerPipeModule(r *Registry) {
	err := r.Register("pipe", pipeModuleOrder, initPipe,
		DeclaresService[*workers.Pipe](),
		DeclaresService[*workers.CallServer]())
	if err != nil {
		panic(err)
	}
}

func initPipe(ctx context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	trust, err := Require[*authz.Trust](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	rt, err := Require[*workers.Router](env)
	if err != nil {
		return err
	}
	ing, err := Require[*workers.Ingest](env)
	if err != nil {
		return err
	}
	keys, err := Require[*apikey.Service](env)
	if err != nil {
		return err
	}
	alog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	log := cfg.logger()
	resolver := settings.Resolver()

	events, err := Require[*workers.EventHub](env)
	if err != nil {
		return err
	}
	p, err := workers.NewPipe(workers.PipeConfig{
		// A session that share_set took away from the share keeps its stored
		// history, hidden: reads and waits ask the Hub's session state.
		Shared: func(workerID, session string) bool { return !events.Fenced(workerID, session) },

		// Retained history is served only while the Worker's CURRENT policy
		// shares it: a connected Worker is asked on its own connection.
		SharedNow: events.HistoryVisible,

		Store:  st.Pipe(),
		Authz:  az,
		Router: rt,
		// A committed message or reply wakes the readers of the session
		// (metadata only, gated on the current share authority).
		Changed: events.PublishPipeChanged,
		Trust:   trust,
		BudgetFunc: func() (int, error) {
			return config.ResolvePipeBudgetPerDay(resolver)
		},
		// A Worker that keeps flooding the turn channel after busy answers
		// loses its connection.
		Abort:  func(workerID string) { svc.Registry().Disconnect(workerID) },
		Logger: log,
	})
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}

	// REST (and MCP, API keys): the handlers of the three ops.
	if err := api.RegisterPipeOps(handlers, p); err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	// Worker call streams: the ops join the call loop of the connect handler.
	cs := workers.NewCallServer(workers.CallServerConfig{
		Keys:     keys,
		Resolver: rt,
		Owners:   svc.OwnerLookup(),
		Authz:    az,
		Auditor:  alog,
		Logger:   log,
	})
	if err := p.RegisterCalls(cs); err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	if err := cs.Bind(svc); err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	// Turn ingestion and turn.ack: the turn channel joins the event ingest.
	if !p.RegisterTurns(ing) {
		return fmt.Errorf("pipe: the event ingest already serves %s streams", workers.FrameTurnsOpen)
	}

	// The prune loop has a context of its own that the stop hook cancels.
	runCtx, cancel := context.WithCancel(context.Background())
	var (
		mu      sync.Mutex
		started bool
		done    = make(chan struct{})
	)
	env.OnStart(func(context.Context) error {
		mu.Lock()
		started = true
		mu.Unlock()
		go func() {
			defer close(done)
			p.RunPrune(runCtx, pipePruneInterval)
		}()
		return nil
	})
	env.OnStop(func(ctx context.Context) error {
		cancel()
		mu.Lock()
		ran := started
		mu.Unlock()
		if !ran {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("the pipe retention loop did not stop: %w", ctx.Err())
		}
	})
	if err := Provide(env, p); err != nil {
		return err
	}
	return Provide(env, cs)
}
