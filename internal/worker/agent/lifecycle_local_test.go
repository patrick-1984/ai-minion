package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func lifecycleSvc(t *testing.T, e *env, counter SessionCounter, runMode string) (*lifecycleService, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	stops, restarts := &atomic.Int32{}, &atomic.Int32{}
	return &lifecycleService{ex: e.ex, counter: counter, runMode: runMode, instance: "i1",
		stop: func() { stops.Add(1) }, restart: func() { restarts.Add(1) }}, stops, restarts
}

func lifecycleCall(s *lifecycleService, a LocalLifecycleArgs) (LifecycleResult, error) {
	raw, err := s.serve(context.Background(), mustJSONNoT(a))
	if err != nil {
		return LifecycleResult{}, err
	}
	var r LifecycleResult
	_ = json.Unmarshal(raw, &r)
	return r, nil
}

func mustJSONNoT(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func assertSessionsListed(t *testing.T, e *env, n int) {
	t.Helper()
	list, err := e.host.ListSessions(context.Background())
	if err != nil || len(list) != n {
		t.Fatalf("the host lists %d sessions (%v), want %d: a refusal ended sessions", len(list), err, n)
	}
}

func TestLifecycleStopRefusesUnknownCount(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("a", "claude")
	s, stops, _ := lifecycleSvc(t, e, SessionCounter{Host: failingListHost{e.host}, EndsSessions: true}, RunModeWindowsService)
	_, err := lifecycleCall(s, LocalLifecycleArgs{Action: LifecycleStop, Instance: "i1"})
	wantAPIError(t, err, "conflict", ReasonCountUnavailable)
	if stops.Load() != 0 {
		t.Fatal("stopped with an unknown count")
	}
	if _, held := e.ex.CreateHeld(); held {
		t.Fatal("the refusal kept the create hold")
	}
	assertSessionsListed(t, e, 1)
	// Accepted explicitly: it stops.
	if r, err := lifecycleCall(s, LocalLifecycleArgs{Action: LifecycleStop, Instance: "i1", AcceptUnknown: true}); err != nil || r.SessionsLost != -1 || !r.Exiting {
		t.Fatalf("accepted unknown = %+v, %v", r, err)
	}
}

func TestLifecycleAcceptLossBoundary(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("a", "claude")
	e.host.addSession("b", "claude")
	s, _, restarts := lifecycleSvc(t, e, SessionCounter{Host: e.host, EndsSessions: true}, RunModeWindowsService)
	_, err := lifecycleCall(s, LocalLifecycleArgs{Action: LifecycleRestart, Instance: "i1", AcceptLoss: 1})
	wantAPIError(t, err, "conflict", ReasonSessionsWouldEnd)
	if ae, _ := AsAPIError(err); ae == nil || !strings.Contains(string(ae.Details), `"sessions_lost":2`) {
		t.Fatalf("details of %v", err)
	}
	assertSessionsListed(t, e, 2)
	if r, err := lifecycleCall(s, LocalLifecycleArgs{Action: LifecycleRestart, Instance: "i1", AcceptLoss: 2}); err != nil || r.SessionsLost != 2 {
		t.Fatalf("accept 2 = %+v, %v", r, err)
	}
	eventuallyTrue(t, func() bool { return restarts.Load() == 1 })
	// A stale instance is refused before anything.
	_, err = lifecycleCall(s, LocalLifecycleArgs{Action: LifecycleStop, Instance: "old"})
	wantAPIError(t, err, "conflict", "state_changed")
}

// A stop that the service manager completes keeps the lifecycle hold; an
// update refusal meanwhile lifts only its own hold.
func TestLifecycleOverlapsUpdateRefusal(t *testing.T) {
	e := newEnv(t)
	s, stops, _ := lifecycleSvc(t, e, SessionCounter{Host: e.host}, RunModeSystemd)
	r, err := lifecycleCall(s, LocalLifecycleArgs{Action: LifecycleStop, Instance: "i1"})
	if err != nil || r.Exiting || stops.Load() != 0 {
		t.Fatalf("systemd stop = %+v, %v, stops %d", r, err, stops.Load())
	}
	e.ex.SetSessionCreateHold("the Worker is updating")
	e.ex.SetSessionCreateHold("") // the update was refused
	_, err = e.write(ops.OpSessionCreate, createArgs("fresh"))
	wantAPIError(t, err, "conflict", "")
}
