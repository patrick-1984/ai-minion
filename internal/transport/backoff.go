package transport

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Reconnect backoff defaults (AD-3): the delay grows from one second to a
// 60 s cap and the Worker never gives up (R78).
const (
	// BackoffInitial is the base delay before the first retry.
	BackoffInitial = time.Second
	// BackoffMax is the cap of the base delay.
	BackoffMax = 60 * time.Second
)

// Backoff produces the delays between reconnect attempts. The base delay
// doubles from Initial up to Max. Each returned delay uses "equal jitter": a
// random value in [base/2, base), so many Workers that lost the Hub at the
// same moment do not reconnect in lockstep, and a delay never exceeds Max. It
// never gives up: Next always returns a delay. The zero value is ready to use
// with the defaults. Safe for concurrent use.
type Backoff struct {
	// Initial is the base delay of the first attempt; zero means
	// BackoffInitial.
	Initial time.Duration
	// Max is the cap of the base delay; zero means BackoffMax.
	Max time.Duration
	// Rand returns a float64 in [0, 1); nil means a random source. Tests
	// pin it to make the sequence deterministic.
	Rand func() float64

	mu      sync.Mutex
	attempt int
}

func (b *Backoff) bounds() (initial, max time.Duration) {
	initial, max = b.Initial, b.Max
	if initial <= 0 {
		initial = BackoffInitial
	}
	if max <= 0 {
		max = BackoffMax
	}
	if initial > max {
		initial = max
	}
	return initial, max
}

// Base returns the un-jittered delay of the given zero-based attempt.
func (b *Backoff) Base(attempt int) time.Duration {
	initial, max := b.bounds()
	d := initial
	for i := 0; i < attempt && d < max; i++ {
		if d > max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// Next returns the delay to wait before the next attempt and advances the
// sequence.
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	attempt := b.attempt
	if b.attempt < 1<<20 { // enough to be at the cap forever, no overflow
		b.attempt++
	}
	b.mu.Unlock()

	base := b.Base(attempt)
	r := rand.Float64
	if b.Rand != nil {
		r = b.Rand
	}
	j := r()
	if j < 0 {
		j = 0
	}
	if j >= 1 {
		j = 0.999999999
	}
	half := base / 2
	return half + time.Duration(float64(base-half)*j)
}

// Reset restarts the sequence; call it after a connection was established.
func (b *Backoff) Reset() {
	b.mu.Lock()
	b.attempt = 0
	b.mu.Unlock()
}

// Wait sleeps for the next delay on clk (SystemClock when nil) and returns
// the delay, or ctx.Err() when the context ends first.
func (b *Backoff) Wait(ctx context.Context, clk Clock) (time.Duration, error) {
	if clk == nil {
		clk = SystemClock
	}
	d := b.Next()
	select {
	case <-clk.After(d):
		return d, nil
	case <-ctx.Done():
		return d, ctx.Err()
	}
}
