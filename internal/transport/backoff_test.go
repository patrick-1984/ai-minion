package transport

import (
	"context"
	"testing"
	"time"
)

func TestBackoffBaseSequenceCapsAt60s(t *testing.T) {
	var b Backoff
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := b.Base(i); got != w*time.Second {
			t.Fatalf("Base(%d) = %v, want %v", i, got, w*time.Second)
		}
	}
	if got := b.Base(1 << 30); got != 60*time.Second {
		t.Fatalf("Base(huge) = %v", got)
	}
}

func TestBackoffNextJitterBoundsAndGrowth(t *testing.T) {
	// Jitter pinned to its extremes: 0 gives base/2, close to 1 gives close to base.
	for _, tc := range []struct {
		name string
		r    float64
		frac float64
	}{{"low", 0, 0.5}, {"high", 0.999999, 1}} {
		b := &Backoff{Rand: func() float64 { return tc.r }}
		for i := 0; i < 12; i++ {
			base := b.Base(i)
			got := b.Next()
			lo := time.Duration(float64(base) * 0.5)
			if got < lo || got > base {
				t.Fatalf("%s attempt %d: %v outside [%v, %v]", tc.name, i, got, lo, base)
			}
			if tc.r == 0 && got != base/2 {
				t.Fatalf("attempt %d: got %v, want %v", i, got, base/2)
			}
		}
	}
}

func TestBackoffNeverExceedsCapAndNeverGivesUp(t *testing.T) {
	b := &Backoff{}
	var prevBase time.Duration
	for i := 0; i < 5000; i++ {
		d := b.Next()
		if d <= 0 || d > BackoffMax {
			t.Fatalf("attempt %d: delay %v out of (0, %v]", i, d, BackoffMax)
		}
		if i > 10 && d < BackoffMax/2 {
			t.Fatalf("attempt %d: delay %v below the jittered cap floor", i, d)
		}
		prevBase = b.Base(i)
	}
	if prevBase != BackoffMax {
		t.Fatalf("base never reached the cap: %v", prevBase)
	}
}

func TestBackoffJitterVaries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		b := &Backoff{}
		for j := 0; j < 8; j++ {
			b.Next()
		}
		seen[b.Next()] = true
	}
	if len(seen) < 5 {
		t.Fatalf("delays at the cap look constant: %d distinct values", len(seen))
	}
}

func TestBackoffReset(t *testing.T) {
	b := &Backoff{Rand: func() float64 { return 0 }}
	for i := 0; i < 5; i++ {
		b.Next()
	}
	b.Reset()
	if got := b.Next(); got != 500*time.Millisecond {
		t.Fatalf("after Reset: %v, want 500ms", got)
	}
}

func TestBackoffCustomBounds(t *testing.T) {
	b := &Backoff{Initial: 10 * time.Second, Max: 15 * time.Second, Rand: func() float64 { return 0 }}
	if got := b.Next(); got != 5*time.Second {
		t.Fatalf("first: %v", got)
	}
	if got := b.Next(); got != 7500*time.Millisecond {
		t.Fatalf("second (capped at 15s): %v", got)
	}
}

func TestBackoffWaitUsesClockAndContext(t *testing.T) {
	clk := newFakeClock()
	b := &Backoff{Rand: func() float64 { return 0 }}
	done := make(chan time.Duration, 1)
	go func() {
		d, err := b.Wait(context.Background(), clk)
		if err != nil {
			t.Error(err)
		}
		done <- d
	}()
	for clk.waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("Wait returned before the clock advanced")
	default:
	}
	clk.Advance(500 * time.Millisecond)
	if d := <-done; d != 500*time.Millisecond {
		t.Fatalf("delay %v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Backoff{}).Wait(ctx, clk); err != context.Canceled {
		t.Fatalf("cancelled Wait: %v", err)
	}
}
