package server

// Automatic app updates (v0.24.0, design WP4): for every Worker whose update
// policy on the Hub is auto (worker_app_update_set), the Hub sends
// worker_update to its own release by itself, once the Worker is online,
// announces self.update and runs an older release. It is made for the
// Android app: the Worker downloads and verifies the APK and hands it to the
// app only when no client is attached and no agent is busy, so an automatic
// update does not interrupt work. The update is sent with the rights of the
// administrator who chose auto, as they are at that moment, and audited
// (actor that administrator, detail auto=true). A failed attempt is not
// repeated for the same release: the next release, or the Update button,
// tries again.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/updates"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// appUpdateModuleOrder runs after the routing module, whose Router it uses.
const appUpdateModuleOrder = routeModuleOrder + 1

// Timings of the automatic updates.
const (
	appUpdateFirst = time.Minute
	appUpdateEvery = 5 * time.Minute
	// appUpdateResend is how long the Hub waits before it sends the same
	// release again to a Worker that has not installed it and reports no
	// update in progress (it restarted, or the request was lost).
	appUpdateResend = 6 * time.Hour
)

func init() {
	if err := DefaultRegistry.Register("app-update", appUpdateModuleOrder, initAppUpdate); err != nil {
		panic(err)
	}
}

func initAppUpdate(_ context.Context, env *Env) error {
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
	rt, err := Require[*workers.Router](env)
	if err != nil {
		return err
	}
	chk, err := Require[*updates.Checker](env)
	if err != nil {
		return err
	}
	alog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	log := cfg.logger()
	a := &appAutoUpdater{
		policies:  st.ListWorkerAppUpdates,
		online:    svc.Registry().List,
		call:      rt.Call,
		published: chk.Published,
		target:    hubReleaseTarget,
		log:       log,
		now:       time.Now,
		audit: func(ctx context.Context, ev audit.Event) {
			if err := alog.Record(context.WithoutCancel(ctx), ev); err != nil {
				log.Error("audit write failed", "action", ev.Action, "error", err)
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := false
	env.OnStart(func(context.Context) error {
		started = true
		go func() { defer close(done); a.run(ctx) }()
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

// hubReleaseTarget is the release Workers are updated to: the Hub's own; a
// development build of the Hub has none.
func hubReleaseTarget() (string, bool) {
	v, err := version.ParseVersion(version.Version)
	if err != nil {
		return "", false
	}
	return v.String(), true
}

type appAutoUpdater struct {
	policies  func(ctx context.Context) (map[string]store.WorkerAppUpdate, error)
	online    func() []workers.Online
	call      func(ctx context.Context, req workers.CallRequest) (any, error)
	published func(tag string) (published, known bool)
	target    func() (string, bool)
	audit     func(ctx context.Context, ev audit.Event)
	log       *slog.Logger
	now       func() time.Time

	mu sync.Mutex
	// sent is the last automatic update sent to each Worker.
	sent map[string]appUpdateSent
}

type appUpdateSent struct {
	target string
	at     time.Time
	// done is set once the attempt for target ended (failed, or the Worker
	// runs target): it is not sent again.
	done bool
}

func (a *appAutoUpdater) run(ctx context.Context) {
	t := time.NewTimer(appUpdateFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.tick(ctx)
		t.Reset(appUpdateEvery)
	}
}

// outdated reports whether build is an older release than target.
func outdated(build, target string) bool {
	b, err1 := version.ParseVersion(build)
	t, err2 := version.ParseVersion(target)
	return err1 == nil && err2 == nil && b.Compare(t) < 0
}

// tick sends the update to every Worker set to auto that needs it.
func (a *appAutoUpdater) tick(ctx context.Context) {
	target, ok := a.target()
	if !ok {
		return
	}
	pols, err := a.policies(ctx)
	if err != nil {
		a.log.Warn("app update: cannot read the update policies", "error", err.Error())
		return
	}
	if len(pols) == 0 {
		return
	}
	// A release the Hub cannot see published is not sent (a draft).
	if pub, known := a.published(target); !known || !pub {
		return
	}
	a.mu.Lock()
	if a.sent == nil {
		a.sent = map[string]appUpdateSent{}
	}
	a.mu.Unlock()
	for _, on := range a.online() {
		p, ok := pols[on.WorkerID]
		if !ok || p.Policy != store.WorkerAppUpdateAuto || on.Pending || on.Facts.OS != "android" {
			continue
		}
		if !proto.HasFeature(on.Features, proto.FeatureSelfUpdate) {
			continue // the Worker says why on its page (update block)
		}
		a.mu.Lock()
		last := a.sent[on.WorkerID]
		a.mu.Unlock()
		if !outdated(on.Facts.Build, target) {
			if last.target == target && !last.done {
				a.record(ctx, p, on.WorkerID, target, "success", "auto=true state=installed")
				a.mark(on.WorkerID, appUpdateSent{target: target, at: last.at, done: true})
			}
			continue
		}
		if last.target == target && (last.done || a.now().Sub(last.at) < appUpdateResend) {
			if !last.done {
				a.follow(ctx, p, on.WorkerID, target, last)
			}
			continue
		}
		a.send(ctx, p, on.WorkerID, target)
	}
}

func (a *appAutoUpdater) mark(id string, s appUpdateSent) {
	a.mu.Lock()
	a.sent[id] = s
	a.mu.Unlock()
}

func (a *appAutoUpdater) principal(p store.WorkerAppUpdate) authz.Principal {
	return authz.Principal{Kind: authz.KindBrowserSession, UserID: p.UpdatedBy}
}

func (a *appAutoUpdater) record(ctx context.Context, p store.WorkerAppUpdate, workerID, target, outcome, detail string) {
	if detail != "" {
		detail += " "
	}
	a.audit(ctx, audit.Event{Actor: p.UpdatedBy, Action: ops.OpWorkerUpdate, Target: workerID, Outcome: outcome,
		Detail: detail + "to=" + target})
}

// send asks the Worker to update to target.
func (a *appAutoUpdater) send(ctx context.Context, p store.WorkerAppUpdate, workerID, target string) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	res, err := a.call(cctx, workers.CallRequest{Principal: a.principal(p), Op: ops.OpWorkerUpdate, WorkerID: workerID,
		Args: ops.WorkerUpdateArgs{WorkerID: workerID, Version: target}})
	if err != nil {
		outcome := "failed"
		var pe *proto.Error
		if errors.As(err, &pe) && (pe.Code == proto.CodeForbidden || pe.Code == proto.CodeNotFound) {
			outcome = "denied"
		}
		a.log.Warn("app update: the automatic update was not sent", "worker_id", workerID, "to", target, "error", err.Error())
		a.record(ctx, p, workerID, target, outcome, "auto=true")
		// Not sent: try again at the resend interval.
		a.mark(workerID, appUpdateSent{target: target, at: a.now()})
		return
	}
	r, _ := res.(ops.WorkerUpdateResult)
	if !r.Accepted {
		a.log.Info("app update: the Worker did not start the automatic update", "worker_id", workerID, "to", target, "reason", r.Reason)
		a.record(ctx, p, workerID, target, "failed", "auto=true state=refused")
		a.mark(workerID, appUpdateSent{target: target, at: a.now()})
		return
	}
	a.log.Info("app update: automatic update sent", "worker_id", workerID, "to", target)
	a.record(ctx, p, workerID, target, "success", "auto=true state=started")
	a.mark(workerID, appUpdateSent{target: target, at: a.now()})
}

// follow asks the Worker about an update sent earlier; a failed one is
// audited and not sent again for that release.
func (a *appAutoUpdater) follow(ctx context.Context, p store.WorkerAppUpdate, workerID, target string, last appUpdateSent) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := a.call(cctx, workers.CallRequest{Principal: a.principal(p), Op: ops.OpWorkerUpdateStatus, WorkerID: workerID,
		Args: ops.WorkerUpdateStatusArgs{WorkerID: workerID}})
	if err != nil {
		return
	}
	st, _ := res.(ops.WorkerUpdateStatus)
	if (st.State == ops.UpdateStateFailed || st.State == ops.UpdateStateRolledBack) && st.To == target {
		a.log.Warn("app update: the automatic update failed", "worker_id", workerID, "to", target, "error", st.Error)
		a.record(ctx, p, workerID, target, "failed", "auto=true state="+st.State)
		a.mark(workerID, appUpdateSent{target: target, at: last.at, done: true})
	}
}
