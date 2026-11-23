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

func (u *usageCollector) sample(ctx context.Context, p authz.Principal, args ops.WorkerUsageArgs) (ops.UsageReport, error) {
	var zero ops.UsageReport
	workerID := args.WorkerID
	owner, err := u.db.WorkerOwner(ctx, workerID)
	if err != nil {
		return zero, err
	}
	cursor, err := u.db.UsageCursor(ctx, workerID)
	if err != nil {
		return zero, err
	}
	args.AfterEvent = cursor
	result, err := u.router.Call(ctx, workers.CallRequest{Principal: p, WorkerID: workerID, Op: ops.OpWorkerUsage, Args: args})
	if err != nil {
		return zero, err
	}
	report, ok := result.(ops.UsageReport)
	if !ok {
		return zero, errors.New("invalid Worker usage report")
	}
	// Return the redacted report: a legacy Worker's endpoint host never leaves the Hub.
	report = report.Sanitize()
	if err = u.db.SaveUsage(ctx, workerID, owner, report, time.Now()); err != nil {
		return zero, err
	}
	return report, nil
}

// inventoryRefresh is how long a Worker's inventory stays current. The Claude
// exporter is installed asynchronously after registration, so while it is not
// installed the Hub asks again soon, and then only now and then for changes.
func inventoryRefresh(inventory []ops.UsageHarness) time.Duration {
	for _, h := range inventory {
		if h.Name == "claude" && h.Status == "installed" && (h.Exporter == ops.UsageExporterAbsent || h.Exporter == "") {
			return time.Minute
		}
	}
	return 10 * time.Minute
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
	inventory := true
	var inventoryDue time.Time
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
		probe, stop := context.WithTimeout(ctx, 25*time.Second)
		// A connection's own Worker identity has read access only to itself.
		// No synthetic owner or administrator credentials are created.
		report, err := u.sample(probe, authz.NewWorkerPrincipal(w.WorkerID), ops.WorkerUsageArgs{WorkerID: w.WorkerID, Inventory: inventory})
		stop()
		<-u.slots
		if err == nil {
			if inventory {
				inventoryDue = time.Now().Add(inventoryRefresh(report.Inventory))
			}
			inventory = !time.Now().Before(inventoryDue)
			u.pollAccount(ctx, w)
			delay = 5 * time.Second
			continue
		}
		status := "unavailable"
		if api.ErrorOf(err).Code == proto.CodeForbidden || w.Facts.UsageConsent == "pending" {
			status = "registration_required"
		}
		_ = u.db.UsageFailure(ctx, w.WorkerID, w.OwnerID, status)
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
		if !ok || target.ID == "" || target.ID != a.WorkerID || a.AccountID != "" {
			return ops.UsageReport{}, api.NewError(proto.CodeForbidden, "usage inventory requires access to the registered Worker")
		}
		return collector.sample(ctx, c.Principal, a)
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
		go func() {
			defer group.Done()
			if w.Facts.UsageConsent == "pending" || w.Facts.UsageConsent == "legacy_pending" {
				_ = st.UsageFailure(run, w.WorkerID, w.OwnerID, "registration_required")

			}
			collector.serve(run, w, sess)
		}()
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

func (u *usageCollector) pollAccount(ctx context.Context, w workers.Online) {
	lease, err := u.db.ClaimUsagePoll(ctx, w.WorkerID, w.OwnerID, time.Now())
	if err != nil || lease.ID == "" {
		return
	}
	query, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	res, err := u.router.Call(query, workers.CallRequest{Principal: authz.NewWorkerPrincipal(w.WorkerID), WorkerID: w.WorkerID, Op: ops.OpWorkerUsage, Args: ops.WorkerUsageArgs{WorkerID: w.WorkerID, AccountID: lease.AccountID}})
	report, ok := res.(ops.UsageReport)
	status := "unavailable"
	var retry time.Duration
	var account *ops.UsageAccount
	if err == nil && ok && report.Check(time.Now()) == nil && report.Probe != nil {
		status = report.Probe.Status
		retry = time.Duration(report.Probe.RetryAfterSeconds) * time.Second
		if status == "ready" && len(report.Accounts) == 1 && report.Accounts[0].ID == lease.AccountID {
			account = &report.Accounts[0]
		} else if status == "ready" {
			status = "unavailable"
		}
	}
	_ = u.db.CompleteUsagePoll(ctx, lease, account, status, retry, time.Now())
}
