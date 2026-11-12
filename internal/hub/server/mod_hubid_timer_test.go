package server

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// The expiry timer closes the sessions at the serving certificate's not_after
// without waiting for the daily tick, and it is re-armed by a re-issue.
func TestExpiryTimerClosesSessionsAtNotAfterAndRearmsAfterReissue(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var mu sync.Mutex
	var off time.Duration
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Add(off) }
	setOff := func(d time.Duration) { mu.Lock(); off = d; mu.Unlock() }
	// The first certificate (180 days) expires 1.5 s from now.
	setOff(-180*24*time.Hour + 1500*time.Millisecond)
	svc := hubidtest.Open(t, st, t.TempDir(), hubidtest.WithNow(now))
	setOff(0) // the certificate was dated by the skewed clock; from here on time is real
	var closes atomic.Int32
	svc.SetSessionCloser(func() { closes.Add(1) })
	base := closes.Load()

	timers := newExpiryTimers(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	timers.start(t.Context())
	svc.SetActivationHook(timers.rearm)
	t.Cleanup(func() { svc.SetActivationHook(nil); timers.stop() })

	waitFor := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for closes.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("sessions closed %d times, want %d", closes.Load(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(base + 1)
	if _, err := svc.Current(); err == nil {
		t.Fatal("the expired certificate still serves")
	}
	// A re-issue of one day, dated so that it expires 1.5 s from now: the
	// activation hook re-arms the timer and the second expiry closes again.
	setOff(-24*time.Hour + 1500*time.Millisecond)
	if _, err := svc.Reissue(t.Context(), hubidtest.Actor, hubidtest.Passphrase, hubid.IssueOptions{ValidityDays: 1}); err != nil {
		t.Fatal(err)
	}
	setOff(0)
	afterReissue := closes.Load() // the activation closed once
	waitFor(afterReissue + 1)
	if _, err := svc.Current(); err == nil {
		t.Fatal("the second certificate still serves after its not_after")
	}
}
