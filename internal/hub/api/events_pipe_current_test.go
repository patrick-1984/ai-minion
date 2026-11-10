package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// pipeChangedProbe answers the Hub's policy probe as the connected Worker would:
// with the rule it holds now, on the current connection.
func pipeChangedProbe(mode string, calls *atomic.Int32, names ...string) func(context.Context, uint64) (ops.ShareProbe, error) {
	return func(_ context.Context, barrier uint64) (ops.ShareProbe, error) {
		calls.Add(1)
		return ops.ShareProbe{Rule: ops.ShareRule{Mode: mode, Names: names}, Revision: "2", OpBarrier: strconv.FormatUint(barrier, 10)}, nil
	}
}

// A pipe_changed queued before a LOCAL unshare on a connected Worker (no
// share_set, no event yet) is dropped at the delivery through the actual
// EventHub (the fresh current-history check of S415); the session the Worker
// still shares is delivered; a failing probe hides (fail closed); a principal
// that may not read the session never causes a probe.
func TestEventsPipeChangedQueuedBeforeAConnectedLocalUnshareIsDropped(t *testing.T) {
	run := func(t *testing.T, probe func(*atomic.Int32) func(context.Context, uint64) (ops.ShareProbe, error), user string) (out string, calls int32) {
		t.Helper()
		r := newEvRig(t, func(c *EventsConfig) { c.Buffer = 8 })
		l, fa := r.feed(wAlice, ss("a", "shell", "stable", ""), ss("keep", "shell", "stable", ""))
		var n atomic.Int32
		l.SetShareProbe(probe(&n))
		gw := &gateWriter{hdr: http.Header{}, gate: make(chan struct{})}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
		req.Header.Set("X-Cred", user)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); r.events.ServeHTTP(gw, req.WithContext(ctx)) }()
		waitFor(t, "the subscription", func() bool { return r.events.Streams() == 1 })
		r.hub.PublishPipeChanged(wAlice, "a")
		r.hub.PublishPipeChanged(wAlice, "keep")
		close(gw.gate)
		_ = fa.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("keep", "shell", "busy", "")})
		waitFor(t, "the queue to be handled", func() bool { return user != "alice" || strings.Contains(gw.text(), "state_changed") })
		cancel()
		<-done
		return gw.text(), n.Load()
	}
	const aEvent = `"worker":"","session":"a"`
	const keepEvent = `"worker":"","session":"keep"`
	t.Run("local-unshare", func(t *testing.T) {
		out, calls := run(t, func(n *atomic.Int32) func(context.Context, uint64) (ops.ShareProbe, error) {
			return pipeChangedProbe("select", n, "keep")
		}, "alice")
		if strings.Contains(out, aEvent) {
			t.Errorf("a wake-up queued before the local unshare was delivered:\n%s", out)
		}
		if !strings.Contains(out, keepEvent) {
			t.Errorf("the wake-up of the session that is still shared was lost:\n%s", out)
		}
		if calls == 0 {
			t.Error("the connected Worker was not asked")
		}
	})
	t.Run("shared-everything-positive", func(t *testing.T) {
		out, _ := run(t, func(n *atomic.Int32) func(context.Context, uint64) (ops.ShareProbe, error) {
			return pipeChangedProbe("all", n)
		}, "alice")
		if !strings.Contains(out, aEvent) || !strings.Contains(out, keepEvent) {
			t.Errorf("an authorised wake-up was not delivered:\n%s", out)
		}
	})
	t.Run("failing-probe-hides", func(t *testing.T) {
		out, _ := run(t, func(n *atomic.Int32) func(context.Context, uint64) (ops.ShareProbe, error) {
			return func(context.Context, uint64) (ops.ShareProbe, error) {
				n.Add(1)
				return ops.ShareProbe{}, errors.New("no answer")
			}
		}, "alice")
		if strings.Contains(out, "event: pipe_changed") {
			t.Errorf("a wake-up was delivered although the policy could not be confirmed:\n%s", out)
		}
	})
	t.Run("foreign-principal-causes-no-probe", func(t *testing.T) {
		out, calls := run(t, func(n *atomic.Int32) func(context.Context, uint64) (ops.ShareProbe, error) {
			return pipeChangedProbe("all", n)
		}, "bob")
		if strings.Contains(out, "event: pipe_changed") {
			t.Errorf("another person got the wake-up:\n%s", out)
		}
		if calls != 0 {
			t.Errorf("the Worker was probed %d times for a principal that may not read the session", calls)
		}
	})
}
