package server

import (
	"context"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func init() { registerGrantsModule(DefaultRegistry) }

func registerGrantsModule(r *Registry) {
	if err := r.Register("grants", sessionControlModuleOrder+4, initGrants); err != nil {
		panic(err)
	}
}

// initGrants binds the grant_* operations (session grants).
func initGrants(_ context.Context, env *Env) error {
	if !catalogHas(ops.Default, ops.OpGrantList) {
		return nil
	}
	handlers, err := Require[*api.Registry](env)
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
	alog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	list := func(ctx context.Context) ([]api.GrantWorker, error) {
		rows, err := st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		online := map[string]workers.Online{}
		for _, o := range svc.Registry().List() {
			online[o.WorkerID] = o
		}
		owners := map[string]string{}
		out := make([]api.GrantWorker, 0, len(rows))
		for _, w := range rows {
			name, ok := owners[w.OwnerID]
			if !ok {
				if u, err := st.UserByID(ctx, w.OwnerID); err == nil {
					name = u.Username
				}
				owners[w.OwnerID] = name
			}
			o, on := online[w.ID]
			out = append(out, api.GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Owner: name,
				Approved: w.ApprovedAt != 0, CanBeSource: canBeMaster(o, on)})
		}
		return out, nil
	}
	if err := api.RegisterGrants(handlers, st, list, az, alog); err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	// The expiry sweep: once at start and then once a day, grants that
	// expired more than grantExpiredKeep ago are deleted.
	run, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := false
	env.OnStart(func(context.Context) error {
		started = true
		go func() { defer close(done); sweepExpiredGrants(run, st, grantSweepEvery) }()
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

const (
	// grantExpiredKeep is how long an expired grant stays listed (as
	// expired) before the sweep deletes it.
	grantExpiredKeep = 30 * 24 * time.Hour
	grantSweepEvery  = 24 * time.Hour
)

// expiredGrantStore is what the sweep needs of the store.
type expiredGrantStore interface {
	DeleteExpiredSessionGrants(ctx context.Context, before int64) (int64, error)
}

// sweepExpiredGrants deletes the long-expired grants now and then every
// interval until ctx ends.
func sweepExpiredGrants(ctx context.Context, st expiredGrantStore, every time.Duration) {
	_, _ = st.DeleteExpiredSessionGrants(ctx, time.Now().Add(-grantExpiredKeep).UnixMilli())
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			_, _ = st.DeleteExpiredSessionGrants(ctx, now.Add(-grantExpiredKeep).UnixMilli())
		}
	}
}
