package workers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeGrantUseStore struct {
	mu    sync.Mutex
	calls []fakeGrantTouch
	fail  error
	wrote chan struct{}
}

type fakeGrantTouch struct {
	at int64
	n  map[string]int64
}

func (f *fakeGrantUseStore) TouchSessionGrants(_ context.Context, ids []string, at int64, n map[string]int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	got := map[string]int64{}
	for _, id := range ids {
		got[id] = n[id]
	}
	f.calls = append(f.calls, fakeGrantTouch{at: at, n: got})
	if f.wrote != nil {
		f.wrote <- struct{}{}
	}
	return nil
}

func (f *fakeGrantUseStore) snapshot() []fakeGrantTouch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeGrantTouch(nil), f.calls...)
}

// fakeGrantClock is a clock and a ticker the test moves by hand.
type fakeGrantClock struct {
	mu      sync.Mutex
	now     time.Time
	ticks   chan time.Time
	asked   time.Duration
	stopped chan struct{}
}

func newFakeGrantClock() *fakeGrantClock {
	return &fakeGrantClock{now: time.UnixMilli(1_000_000), ticks: make(chan time.Time), stopped: make(chan struct{})}
}

func (c *fakeGrantClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeGrantClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeGrantClock) ticker(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	c.asked = d
	c.mu.Unlock()
	return c.ticks, func() { close(c.stopped) }
}

func TestGrantUseBatchesAndFlushesOnTheTick(t *testing.T) {
	st := &fakeGrantUseStore{wrote: make(chan struct{}, 4)}
	clk := newFakeGrantClock()
	g := NewGrantUse(st, WithGrantUseClock(clk.Now), WithGrantUseTicker(clk.ticker))
	g.Start()
	g.Start() // a second start does nothing

	g.Used("g1", "g2")
	clk.advance(30 * time.Second)
	g.Used("g1", "")
	if n := len(st.snapshot()); n != 0 {
		t.Fatalf("%d writes before the tick", n)
	}
	clk.advance(30 * time.Second)
	clk.ticks <- clk.Now()
	select {
	case <-st.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("no write after the tick")
	}
	calls := st.snapshot()
	if len(calls) != 1 || calls[0].n["g1"] != 2 || calls[0].n["g2"] != 1 || len(calls[0].n) != 2 || calls[0].at != 1_030_000 {
		t.Fatalf("calls = %+v", calls)
	}
	clk.mu.Lock()
	asked := clk.asked
	clk.mu.Unlock()
	if asked != GrantUseFlushEvery || GrantUseFlushEvery != 60*time.Second {
		t.Fatalf("ticker interval = %v", asked)
	}

	// Close writes what is left and ends the loop.
	clk.advance(time.Second)
	g.Used("g3")
	if err := g.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clk.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker was not stopped: the loop leaked")
	}
	calls = st.snapshot()
	if len(calls) != 2 || calls[1].n["g3"] != 1 || calls[1].at != 1_061_000 {
		t.Fatalf("after close = %+v", calls)
	}
	if err := g.Close(t.Context()); err != nil { // idempotent
		t.Fatal(err)
	}
	if n := len(st.snapshot()); n != 2 {
		t.Fatalf("second close wrote: %d calls", n)
	}
}

func TestGrantUseKeepsAFailedBatch(t *testing.T) {
	st := &fakeGrantUseStore{fail: errors.New("busy")}
	clk := newFakeGrantClock()
	g := NewGrantUse(st, WithGrantUseClock(clk.Now), WithGrantUseTicker(clk.ticker))
	g.Used("g1")
	if err := g.Flush(t.Context()); err == nil {
		t.Fatal("flush hid the store error")
	}
	g.Used("g1")
	st.mu.Lock()
	st.fail = nil
	st.mu.Unlock()
	// Close without Start: no loop, the batch is still written.
	if err := g.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := st.snapshot()
	if len(calls) != 1 || calls[0].n["g1"] != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	g.Start() // after Close: nothing starts
	select {
	case <-clk.stopped:
		t.Fatal("a loop ran after Close")
	default:
	}
}

func TestGrantUseCloseHonoursItsContext(t *testing.T) {
	block := make(chan struct{})
	st := &blockingGrantUseStore{entered: make(chan struct{}), release: block}
	clk := newFakeGrantClock()
	g := NewGrantUse(st, WithGrantUseClock(clk.Now), WithGrantUseTicker(clk.ticker))
	g.Start()
	g.Used("g1")
	clk.ticks <- clk.Now()
	<-st.entered // the loop is inside a write that does not return
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close while the store hangs = %v", err)
	}
	close(block)
	if err := g.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var nilUse *GrantUse
	nilUse.Used("g1")
	nilUse.Start()
	if err := nilUse.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type blockingGrantUseStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingGrantUseStore) TouchSessionGrants(context.Context, []string, int64, map[string]int64) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}
