package server

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// A subscription probe is not part of the five-second session sample. Only
// cached, local metadata and event counters belong in a normal Worker report.
type usageCollector struct {
	db     *store.Store
	router *workers.Router
	slots  chan struct{}
}

func (u *usageCollector) sample(ctx context.Context, p authz.Principal, workerID string, inventory bool) (ops.UsageReport, error) {
	var zero ops.UsageReport
	owner, err := u.db.WorkerOwner(ctx, workerID)
	if err != nil {
		return zero, err
	}
	result, err := u.router.Call(ctx, workers.CallRequest{Principal: p, WorkerID: workerID, Op: ops.OpWorkerUsage, Args: ops.WorkerUsageArgs{WorkerID: workerID, Inventory: inventory}})
	if err != nil {
		return zero, err
	}
	report, ok := result.(ops.UsageReport)
	if !ok {
		return zero, errors.New("invalid Worker usage report")
	}
	if err = u.db.SaveUsage(ctx, workerID, owner, report, time.Now()); err != nil {
		return zero, err
	}
	return report, nil
}

func (u *usageCollector) serve(ctx context.Context, w workers.Online, sess workers.OpSession) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-sess.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	delay := time.Duration(0)
	for ctx.Err() == nil {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		select {
		case <-ctx.Done():
			return
		case u.slots <- struct{}{}:
		}
		probe, stop := context.WithTimeout(ctx, 8*time.Second)
		// A connection's own Worker identity has read access only to itself.
		// No synthetic owner or administrator credentials are created.
		_, err := u.sample(probe, authz.NewWorkerPrincipal(w.WorkerID), w.WorkerID, false)
		stop()
		<-u.slots
		if err == nil {
			delay = 5 * time.Second
			continue
		}
		delay = min(5*time.Minute, max(5*time.Second, delay*2)) + time.Duration(rand.IntN(1000))*time.Millisecond
	}
}

func registerUsageCollection(ctx context.Context, env *Env, st *store.Store, r *api.Registry, rt *workers.Router) error {
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	collector := &usageCollector{db: st, router: rt, slots: make(chan struct{}, 8)}
	if err := api.HandleTyped(r, ops.OpWorkerUsage, func(ctx context.Context, c *api.Call, a ops.WorkerUsageArgs) (ops.UsageReport, error) {
		// Even when a session key can read a single session's resource, the
		// caller must independently have read on the whole requested Worker.
		target, ok := c.Resource.(authz.WorkerResource)
		if !ok || target.ID == "" || target.ID != a.WorkerID {
			return ops.UsageReport{}, api.NewError(proto.CodeForbidden, "usage inventory requires access to the registered Worker")
		}
		return collector.sample(ctx, c.Principal, target.ID, a.Inventory)
	}); err != nil {
		return err
	}

	run, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var group sync.WaitGroup
	svc.Registry().OnAdd(func(w workers.Online, sess workers.OpSession) {
		mu.Lock()
		defer mu.Unlock()
		if run.Err() != nil || w.Pending || !w.Announces(ops.OpWorkerUsage) {
			return
		}
		group.Add(1)
		go func() { defer group.Done(); collector.serve(run, w, sess) }()
	})
	group.Add(1)
	go func() {
		defer group.Done()
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-run.Done():
				return
			case now := <-tick.C:
				_ = st.TrimUsage(run, now)
			}
		}
	}()
	env.OnStop(func(context.Context) error { mu.Lock(); cancel(); mu.Unlock(); group.Wait(); return nil })
	return nil
}
