package server

// Vanished sessions lose their control settings (v0.9.1, A20): this module
// follows the Hub's event feed. A session_removed (the session ended or stopped
// being shared) purges that session; a worker_resynced (a Worker connected or
// sent a fresh snapshot, also after a Hub restart) purges every session the
// Worker no longer lists. See store.PurgeGoneSessions.

import (
	"context"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

const sessionPurgeModuleOrder = eventsModuleOrder + 2

func init() {
	if err := DefaultRegistry.Register("session_purge", sessionPurgeModuleOrder, initSessionPurge); err != nil {
		panic(err)
	}
}

func initSessionPurge(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	hub, err := Require[*workers.EventHub](env)
	if err != nil {
		return err
	}
	p := &sessionPurger{st: st, hub: hub, sessions: hub, log: cfg.logger()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := false
	env.OnStart(func(context.Context) error {
		started = true
		go func() { defer close(done); p.run(ctx) }()
		return nil
	})
	env.OnStop(func(stop context.Context) error {
		cancel()
		if !started {
			return nil
		}
		select {
		case <-done:
		case <-stop.Done():
		}
		return nil
	})
	return nil
}

type sessionPurger struct {
	st  *store.Store
	hub *workers.EventHub
	// sessions is the hub's view of a Worker's sessions (a fake in tests).
	sessions interface {
		Sessions(workerID string) (workers.WorkerSessions, bool)
		Fenced(workerID, session string) bool
	}
	log interface {
		Info(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// run follows the feed until ctx ends; a subscription closed for being slow
// is opened again. Only a resync event triggers the "not listed" purge: it
// comes after the Worker's snapshot, never before it.
func (p *sessionPurger) run(ctx context.Context) {
	for ctx.Err() == nil {
		sub := p.hub.Subscribe(1024)
		p.follow(ctx, sub)
		sub.Close()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (p *sessionPurger) follow(ctx context.Context, sub *workers.Subscription) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C():
			if !ok {
				return
			}
			p.handle(ctx, ev)
		}
	}
}

// handle purges after one event.
func (p *sessionPurger) handle(ctx context.Context, ev workers.Event) {
	switch ev.Type {
	case workers.EventSessionRemoved:
		if n, err := p.st.PurgeGoneSessions(ctx, ev.WorkerID, []string{ev.Session}); err != nil {
			p.log.Error("purge the control settings of a gone session", "worker_id", ev.WorkerID, "error", err)
		} else if n > 0 {
			p.log.Info("a gone session lost its control settings", "worker_id", ev.WorkerID, "session", ev.Session)
		}
	case workers.EventWorkerResynced:
		p.resynced(ctx, ev.WorkerID)
	}
}

// resynced purges the sessions the Worker no longer lists (its shared ones).
func (p *sessionPurger) resynced(ctx context.Context, workerID string) {
	ws, ok := p.sessions.Sessions(workerID)
	if !ok || !ws.Online {
		return
	}
	live := make([]string, 0, len(ws.Sessions))
	for _, s := range ws.Sessions {
		if !p.sessions.Fenced(workerID, s.Name) {
			live = append(live, s.Name)
		}
	}
	if n, err := p.st.PurgeSessionsNotIn(ctx, workerID, live); err != nil {
		p.log.Error("purge the control settings of gone sessions", "worker_id", workerID, "error", err)
	} else if n > 0 {
		p.log.Info("gone sessions lost their control settings", "worker_id", workerID, "rows", n)
	}
}
