package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
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
	r, err := usageSnapshot(context.Background(), usageHostFixture{sessions}, false, "")
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

// A report whose quota detail is too large keeps its sessions: the quota
// windows are dropped, not the report.
func TestFitUsageReportDropsOversizedQuotaDetailNotTheReport(t *testing.T) {
	now := time.Now()
	at := now.UnixMilli()
	var windows []ops.UsageWindow
	for i := range 32 {
		windows = append(windows, ops.UsageWindow{Scope: fmt.Sprintf("model-%02d-%060d", i, 0), Period: "rolling", UsedPercent: new(20.)})
	}
	account := ops.UsageAccount{ID: ops.UsageID("big", "account"), Provider: "Anthropic", AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, Windows: windows}
	r := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: at, Accounts: []ops.UsageAccount{account}}
	for i := range 200 {
		r.Sessions = append(r.Sessions, ops.UsageSession{ID: ops.UsageID("big", fmt.Sprint(i)), Name: fmt.Sprint("s", i), AuthMode: "api", CustomEndpoint: true, Source: "claude-statusline", ObservedAt: at, AccountID: account.ID, Windows: windows})
	}
	if err := fitUsageReport(&r, now); err != nil {
		t.Fatalf("an oversized but valid report was rejected: %v", err)
	}
	if len(r.Sessions) != 200 {
		t.Fatalf("sessions dropped: %d", len(r.Sessions))
	}
}
