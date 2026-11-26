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
		All() []workers.WorkerSessions
	}
	log interface {
		Info(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// run follows the feed until ctx ends. A subscription closed for being slow
// is opened again and every Worker that sent its snapshot is checked, so a
// removal missed in the gap is still applied.
func (p *sessionPurger) run(ctx context.Context) {
	for ctx.Err() == nil {
		sub := p.hub.Subscribe(1024)
		p.reconcile(ctx)
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
		// One transaction: the control settings and the grants that name the
		// session, each grant with its audit row (cause session_gone). A
		// failed audit insert rolls the whole purge back; a later resync
		// of the Worker retries it.
		if n, err := p.st.PurgeGoneSessions(ctx, ev.WorkerID, []string{ev.Session}, asOf(ev)); err != nil {
			p.log.Error("purge the control settings and grants of a gone session", "worker_id", ev.WorkerID, "error", err)
		} else if n > 0 {
			p.log.Info("a gone session lost its control settings and grants", "worker_id", ev.WorkerID, "session", ev.Session, "rows", n)
		}
	case workers.EventWorkerResynced:
		// The snapshot's own list: a session added after it (a new terminal
		// under an old name) must not keep the old settings.
		if names, ok := ev.SnapshotNames(); ok {
			p.purgeNotIn(ctx, ev.WorkerID, names, asOf(ev))
			return
		}
		p.resynced(ctx, ev.WorkerID)
	}
}

// reconcile checks every Worker that sent its snapshot on this connection.
func (p *sessionPurger) reconcile(ctx context.Context) {
	for _, ws := range p.sessions.All() {
		if ws.Synced {
			p.resynced(ctx, ws.WorkerID)
		}
	}
}

// resynced purges the sessions the Worker no longer lists. The list is the
// Worker's snapshot of this connection, which holds only the sessions it
// shares (the Hub drops hidden ones when it applies the snapshot); before
// that snapshot nothing is purged.
func (p *sessionPurger) resynced(ctx context.Context, workerID string) {
	ws, ok := p.sessions.Sessions(workerID)
	if !ok || !ws.Synced {
		return
	}
	live := make([]string, 0, len(ws.Sessions))
	for _, s := range ws.Sessions {
		live = append(live, s.Name)
	}
	p.purgeNotIn(ctx, workerID, live, ws.UpdatedAt.UnixMilli())
}

// asOf is the time of an event's evidence in unix milliseconds.
func asOf(ev workers.Event) int64 { return ev.Time.Time().UnixMilli() }

func (p *sessionPurger) purgeNotIn(ctx context.Context, workerID string, live []string, at int64) {
	if n, err := p.st.PurgeSessionsNotIn(ctx, workerID, live, at); err != nil {
		p.log.Error("purge the control settings and grants of gone sessions", "worker_id", workerID, "error", err)
	} else if n > 0 {
		p.log.Info("gone sessions lost their control settings and grants", "worker_id", workerID, "rows", n)
	}
}
