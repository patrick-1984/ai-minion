package agent

import (
	"context"
	"log/slog"
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
