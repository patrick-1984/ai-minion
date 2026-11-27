package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The automatic update goes only to an online Android Worker set to auto
// that announces self.update and is older, with the rights of the
// administrator who chose auto; it is audited, not sent twice, and a failed
// attempt is not repeated for the same release.
func TestAppAutoUpdater(t *testing.T) {
	android := func(id, build string, features ...string) workers.Online {
		return workers.Online{WorkerID: id, Features: features, Facts: transport.WorkerFacts{OS: "android", Build: build}}
	}
	online := []workers.Online{
		android("tv", "v0.23.0", proto.FeatureSelfUpdate),
		android("manual", "v0.23.0", proto.FeatureSelfUpdate),
		android("blocked", "v0.23.0"),
		android("current", "v0.24.0", proto.FeatureSelfUpdate),
		{WorkerID: "linux", Features: []string{proto.FeatureSelfUpdate}, Facts: transport.WorkerFacts{OS: "linux", Build: "v0.23.0"}},
	}
	pols := map[string]store.WorkerAppUpdate{}
	for _, id := range []string{"tv", "blocked", "current", "linux"} {
		pols[id] = store.WorkerAppUpdate{WorkerID: id, Policy: store.WorkerAppUpdateAuto, UpdatedBy: "admin-1"}
	}
	var calls []workers.CallRequest
	var events []audit.Event
	status := ops.WorkerUpdateStatus{State: ops.UpdateStateWaitingIdle, To: "v0.24.0"}
	now := time.Unix(1_800_000_000, 0)
	a := &appAutoUpdater{
		policies:  func(context.Context) (map[string]store.WorkerAppUpdate, error) { return pols, nil },
		online:    func() []workers.Online { return online },
		published: func(string) (bool, bool) { return true, true },
		target:    func() (string, bool) { return "v0.24.0", true },
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:       func() time.Time { return now },
		audit:     func(_ context.Context, ev audit.Event) { events = append(events, ev) },
		call: func(_ context.Context, req workers.CallRequest) (any, error) {
			calls = append(calls, req)
			if req.Op == ops.OpWorkerUpdateStatus {
				return status, nil
			}
			return ops.WorkerUpdateResult{Accepted: true, From: "v0.23.0", To: "v0.24.0"}, nil
		},
	}
	ctx := context.Background()
	a.tick(ctx)
	if len(calls) != 1 || calls[0].WorkerID != "tv" || calls[0].Op != ops.OpWorkerUpdate || calls[0].Principal.UserID != "admin-1" {
		t.Fatalf("calls = %+v", calls)
	}
	if args := calls[0].Args.(ops.WorkerUpdateArgs); args.Version != "v0.24.0" || args.Now {
		t.Fatalf("args = %+v", args)
	}
	if len(events) != 1 || events[0].Actor != "admin-1" || events[0].Target != "tv" || events[0].Detail != "auto=true state=started to=v0.24.0" {
		t.Fatalf("events = %+v", events)
	}
	// Next tick: still waiting, only a status question.
	calls = nil
	a.tick(ctx)
	if len(calls) != 1 || calls[0].Op != ops.OpWorkerUpdateStatus {
		t.Fatalf("second tick calls = %+v", calls)
	}
	// It failed: audited once, never sent again for this release.
	status = ops.WorkerUpdateStatus{State: ops.UpdateStateFailed, To: "v0.24.0", Error: "declined"}
	calls, events = nil, nil
	a.tick(ctx)
	now = now.Add(appUpdateResend * 2)
	a.tick(ctx)
	if len(events) != 1 || events[0].Outcome != "failed" || len(calls) != 1 {
		t.Fatalf("after failure: calls %+v events %+v", calls, events)
	}
	// Not published: nothing is sent.
	a.published = func(string) (bool, bool) { return false, true }
	a.sent, calls = nil, nil
	a.tick(ctx)
	if len(calls) != 0 {
		t.Fatalf("draft: calls = %+v", calls)
	}
}
