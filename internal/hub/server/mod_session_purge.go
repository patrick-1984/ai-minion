package server

// Vanished sessions lose their control settings (v0.9.1, A20): this module
// follows the Hub's event feed. A session_removed (the session ended or stopped
// being shared) purges that session; a worker_resynced (a Worker connected or
// sent a fresh snapshot, also after a Hub restart) purges every session the
// Worker no longer lists. A session first seen after the Hub already listed
// the Worker (a new terminal, also one that took a gone session's name) loses
// what was set up before it was first seen: the name-only grants of the
// session it replaced never reach it (workers.CallServerConfig.Sessions) and
// are deleted here, so that a Hub restart does not bring them back. A purge
// that fails is retried: the feed is opened again and every Worker checked.
// See store.PurgeGoneSessions.

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
	p := &sessionPurger{st: st, hub: hub, sessions: hub, seen: hub, log: cfg.logger()}
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
	// seen tells when the hub first observed a session (*workers.EventHub;
	// nil in tests that do not need it).
	seen workers.SessionGenerations
	log  interface {
		Info(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// run follows the feed until ctx ends. A subscription closed for being slow,
// or after a purge that failed, is opened again and every Worker that sent
// its snapshot is checked, so a removal missed in the gap is still applied.
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
			if err := p.handle(ctx, ev); err != nil {
				return // retried by the check of every Worker
			}
		}
	}
}

// handle purges after one event; an error means a purge did not commit.
func (p *sessionPurger) handle(ctx context.Context, ev workers.Event) error {
	switch ev.Type {
	case workers.EventSessionRemoved:
		// One transaction: the control settings and the grants that name the
		// session, each grant with its audit row (cause session_gone). A
		// failed audit insert rolls the whole purge back; it is retried.
		return p.purge(ctx, ev.WorkerID, ev.Session, asOf(ev))
	case workers.EventSessionAdded, workers.EventStateChanged:
		// The event that made the hub list the session (its first-seen time).
		return p.purgeReplaced(ctx, ev.WorkerID, []string{ev.Session}, asOf(ev))
	case workers.EventWorkerResynced:
		// The snapshot's own list: a session added after it (a new terminal
		// under an old name) must not keep the old settings.
		if names, ok := ev.SnapshotNames(); ok {
			if err := p.purgeNotIn(ctx, ev.WorkerID, names, asOf(ev)); err != nil {
				return err
			}
			return p.purgeReplaced(ctx, ev.WorkerID, names, asOf(ev))
		}
		return p.resynced(ctx, ev.WorkerID)
	}
	return nil
}

// purgeReplaced purges, for each of names that the hub first observed at
// the time at (or at any time after it listed the Worker when at is 0), what
// was set up for the name before that time. A session listed since before
// the hub could observe it (first-seen time 0) is left alone.
func (p *sessionPurger) purgeReplaced(ctx context.Context, workerID string, names []string, at int64) error {
	if p.seen == nil {
		return nil
	}
	for _, n := range names {
		first, ok := p.seen.FirstSeen(workerID, n)
		if !ok || first == 0 || at != 0 && first != at {
			continue
		}
		if err := p.purge(ctx, workerID, n, first); err != nil {
			return err
		}
	}
	return nil
}

func (p *sessionPurger) purge(ctx context.Context, workerID, session string, at int64) error {
	n, err := p.st.PurgeGoneSessions(ctx, workerID, []string{session}, at)
	if err != nil {
		p.log.Error("purge the control settings and grants of a gone session", "worker_id", workerID, "error", err)
		return err
	}
	if n > 0 {
		p.log.Info("a gone session lost its control settings and grants", "worker_id", workerID, "session", session, "rows", n)
	}
	return nil
}

// reconcile checks every Worker that sent its snapshot on this connection.
func (p *sessionPurger) reconcile(ctx context.Context) {
	for _, ws := range p.sessions.All() {
		if ws.Synced {
			_ = p.resynced(ctx, ws.WorkerID)
		}
	}
}

// resynced purges the sessions the Worker no longer lists. The list is the
// Worker's snapshot of this connection, which holds only the sessions it
// shares (the Hub drops hidden ones when it applies the snapshot); before
// that snapshot nothing is purged.
func (p *sessionPurger) resynced(ctx context.Context, workerID string) error {
	ws, ok := p.sessions.Sessions(workerID)
	if !ok || !ws.Synced {
		return nil
	}
	live := make([]string, 0, len(ws.Sessions))
	for _, s := range ws.Sessions {
		live = append(live, s.Name)
	}
	if err := p.purgeNotIn(ctx, workerID, live, ws.UpdatedAt.UnixMilli()); err != nil {
		return err
	}
	return p.purgeReplaced(ctx, workerID, live, 0)
}

// asOf is the time of an event's evidence in unix milliseconds.
func asOf(ev workers.Event) int64 { return ev.Time.Time().UnixMilli() }

func (p *sessionPurger) purgeNotIn(ctx context.Context, workerID string, live []string, at int64) error {
	n, err := p.st.PurgeSessionsNotIn(ctx, workerID, live, at)
	if err != nil {
		p.log.Error("purge the control settings and grants of gone sessions", "worker_id", workerID, "error", err)
		return err
	}
	if n > 0 {
		p.log.Info("gone sessions lost their control settings and grants", "worker_id", workerID, "rows", n)
	}
	return nil
}
