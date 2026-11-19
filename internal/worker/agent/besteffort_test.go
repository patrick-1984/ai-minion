package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/logx"
)

type stuckHandler struct{ release chan struct{} }

func (stuckHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h stuckHandler) Handle(context.Context, slog.Record) error {
	<-h.release
	return nil
}
func (h stuckHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h stuckHandler) WithGroup(string) slog.Handler      { return h }

// A shutdown message on an output that never returns does not hold the stop
// sequence.
func TestShutdownMessageIsBestEffort(t *testing.T) {
	h := stuckHandler{make(chan struct{})}
	defer close(h.release)
	start := time.Now()
	logx.BestEffort(slog.New(h), slog.LevelInfo, "worker shutting down")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("blocked for %s", d)
	}
}

type gatedHandler struct {
	stuck   *atomic.Bool
	release chan struct{}
}

func (gatedHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h gatedHandler) Handle(context.Context, slog.Record) error {
	if h.stuck.Load() {
		<-h.release
	}
	return nil
}
func (h gatedHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h gatedHandler) WithGroup(string) slog.Handler      { return h }

// The per-module stop records, on a stalled debug output, stay within the
// hard stop deadline however many modules there are.
func TestModuleStopLogsStayWithinTheStopBudget(t *testing.T) {
	stuck := &atomic.Bool{}
	h := gatedHandler{stuck, make(chan struct{})}
	defer close(h.release)
	l := &eventLog{}
	r := NewRegistry()
	for i, n := range []string{"a", "b", "c", "d", "e", "f"} {
		fakeModule(t, r, l, n, i)
	}
	rt := NewRuntime(r, Options{StopTimeout: 100 * time.Millisecond})
	s := testSettings()
	s.Logger = slog.New(h)
	if err := rt.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	stuck.Store(true)
	began := time.Now()
	_ = rt.Stop(context.Background())
	if took := time.Since(began); took > 350*time.Millisecond {
		t.Fatalf("Stop took %v with a 100ms budget", took)
	}
	if len(l.get()) != 12 {
		t.Fatalf("events %v", l.get())
	}
}
