package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

type blockingProcs struct{ calls atomic.Int32 }

func (b *blockingProcs) Processes(ctx context.Context) ([]agentdetect.Proc, error) {
	b.calls.Add(1)
	<-make(chan struct{}) // ignores the context: a stalled provider
	return nil, nil
}

func TestUsageSnapshotTakesOneBoundedProcessQuery(t *testing.T) {
	oldD, oldT := usageDetector, usageProcTimeout
	t.Cleanup(func() { usageDetector, usageProcTimeout = oldD, oldT })
	src := &blockingProcs{}
	usageDetector = agentdetect.New(agentdetect.WithProcessSource(src))
	usageProcTimeout = 200 * time.Millisecond
	var sessions []term.Session
	for i := 0; i < 6; i++ {
		sessions = append(sessions, term.Session{Name: string(rune('a' + i)), Panes: []term.Pane{{Active: true, Ref: term.PaneRef{Command: "bash", PID: 100 + i}}}})
	}
	start := time.Now()
	r, err := usageSnapshot(context.Background(), usageHostFixture{sessions}, false)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("snapshot took %v", d)
	}
	if len(r.Sessions) != 6 {
		t.Fatalf("sessions = %d", len(r.Sessions))
	}
	for _, s := range r.Sessions {
		if s.Harness != "" || s.AuthMode != "unknown" {
			t.Fatalf("expected basic metadata: %+v", s)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("process queries = %d, want 1", n)
	}
}
