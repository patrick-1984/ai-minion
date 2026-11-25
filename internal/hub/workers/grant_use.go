package workers

// Grant usage: which grants allowed a call, how often and when last. The
// call path only notes the grant id in memory; the counts reach the store in
// one write per interval, so a busy grant costs no write per call.

import (
	"context"
	"sync"
	"time"
)

// GrantUseFlushEvery is how often the noted uses are written to the store.
const GrantUseFlushEvery = 60 * time.Second

// grantUseFlushTimeout bounds one write of the noted uses.
const grantUseFlushTimeout = 10 * time.Second

// GrantUseStore is what the recorder needs; *store.Store implements it.
type GrantUseStore interface {
	TouchSessionGrants(ctx context.Context, ids []string, at int64, n map[string]int64) error
}

// GrantUse batches the uses of grants and writes them every
// [GrantUseFlushEvery] and on Close. The zero value is not usable; a nil
// *GrantUse records nothing.
type GrantUse struct {
	st    GrantUseStore
	now   func() time.Time
	every time.Duration
	// ticker returns the flush ticks and a function that stops them.
	ticker func(time.Duration) (<-chan time.Time, func())

	mu      sync.Mutex
	pending map[string]int64
	last    int64 // unix milliseconds of the latest noted use

	startOnce sync.Once
	closeOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// GrantUseOption changes how [NewGrantUse] builds the recorder (tests).
type GrantUseOption func(*GrantUse)

// WithGrantUseClock sets the clock that stamps the uses.
func WithGrantUseClock(now func() time.Time) GrantUseOption {
	return func(g *GrantUse) { g.now = now }
}

// WithGrantUseTicker replaces the flush ticker.
func WithGrantUseTicker(ticker func(time.Duration) (<-chan time.Time, func())) GrantUseOption {
	return func(g *GrantUse) { g.ticker = ticker }
}

// NewGrantUse returns a recorder that writes to st. Nothing runs until
// [GrantUse.Start].
func NewGrantUse(st GrantUseStore, opts ...GrantUseOption) *GrantUse {
	g := &GrantUse{
		st: st, now: time.Now, every: GrantUseFlushEvery,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		pending: map[string]int64{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Used notes one use of each named grant. It never blocks on the store.
func (g *GrantUse) Used(ids ...string) {
	if g == nil || len(ids) == 0 {
		return
	}
	at := g.now().UnixMilli()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range ids {
		if id != "" {
			g.pending[id]++
		}
	}
	if at > g.last {
		g.last = at
	}
}

// Start begins the periodic flush. A second call does nothing; after Close
// it does nothing either.
func (g *GrantUse) Start() {
	if g == nil {
		return
	}
	g.startOnce.Do(func() {
		select {
		case <-g.stop:
			close(g.done)
			return
		default:
		}
		go g.run()
	})
}

func (g *GrantUse) run() {
	defer close(g.done)
	ticks, stopTicks := g.ticker(g.every)
	defer stopTicks()
	for {
		select {
		case <-g.stop:
			return
		case <-ticks:
			ctx, cancel := context.WithTimeout(context.Background(), grantUseFlushTimeout)
			_ = g.Flush(ctx) // a failed write is kept and tried again
			cancel()
		}
	}
}

// Flush writes the uses noted so far. When the write fails they are kept for
// the next flush.
func (g *GrantUse) Flush(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	batch, at := g.pending, g.last
	g.pending = map[string]int64{}
	g.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	ids := make([]string, 0, len(batch))
	for id := range batch {
		ids = append(ids, id)
	}
	if err := g.st.TouchSessionGrants(ctx, ids, at, batch); err != nil {
		g.mu.Lock()
		for id, n := range batch {
			g.pending[id] += n
		}
		g.mu.Unlock()
		return err
	}
	return nil
}

// Close stops the periodic flush, waits for it to end (or for ctx) and
// writes what is still noted. It is safe to call more than once and without
// Start.
func (g *GrantUse) Close(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.closeOnce.Do(func() { close(g.stop) })
	g.Start() // without a loop this only closes done
	select {
	case <-g.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.Flush(ctx)
}
