package client

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The monitoring header is informational: the call goes through, the notice is
// shown once, and "off" or an unknown value shows nothing.
func TestHubMonitoringNoticeIsInformationalAndOnce(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   int
	}{{"full", 1}, {"actions", 1}, {"off", 0}, {"", 0}, {"FULL", 0}} {
		f := newHubFixture(t, func(_ *hubFixture, w http.ResponseWriter, _ *http.Request) {
			if tc.header != "" {
				w.Header().Set(MonitoringHeader, tc.header)
			}
			jsonReply(w, 200, `{"sessions":[]}`)
		})
		var mu sync.Mutex
		var notices []string
		b := f.hub(t, WithHubNotice(func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }))
		for i := 0; i < 3; i++ {
			if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
				t.Fatalf("%q: %v", tc.header, err)
			}
		}
		mu.Lock()
		n := len(notices)
		mu.Unlock()
		if n != tc.want {
			t.Errorf("header %q: %d notices, want %d", tc.header, n, tc.want)
		}
	}
}

// A stalled notice writer holds a response for a bounded time only, and a
// writer that is fast is waited for (a command that exits right after prints it).
func TestDeliverNoticeIsBounded(t *testing.T) {
	stall := make(chan struct{})
	defer close(stall)
	start := time.Now()
	deliverNotice(func(string) { <-stall }, "x")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v for a stalled writer", d)
	}
	var wrote bool
	deliverNotice(func(string) { wrote = true }, "x")
	if !wrote {
		t.Fatal("a fast writer was not waited for")
	}
}
