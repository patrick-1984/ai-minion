package transport

import (
	"sort"
	"sync"
	"time"
)

// fakeClock is a manually advanced Clock. Advance fires every ticker and
// timer that falls due, in time order, and hands each tick to its consumer
// before moving on, so tests are deterministic without sleeping.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
	afters  []*fakeAfter
}

type fakeTicker struct {
	clk    *fakeClock
	period time.Duration
	next   time.Time
	ch     chan time.Time
	stop   chan struct{}
	once   sync.Once
}

type fakeAfter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTicker(d time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTicker{clk: c, period: d, next: c.now.Add(d), ch: make(chan time.Time), stop: make(chan struct{})}
	c.tickers = append(c.tickers, t)
	return t
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := &fakeAfter{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.afters = append(c.afters, a)
	return a.ch
}

func (t *fakeTicker) C() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()               { t.once.Do(func() { close(t.stop) }) }

// waiters returns the number of live tickers and pending timers.
func (c *fakeClock) waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.afters)
	for _, t := range c.tickers {
		select {
		case <-t.stop:
		default:
			n++
		}
	}
	return n
}

// Advance moves time forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []time.Time
		for _, t := range c.tickers {
			select {
			case <-t.stop:
				continue
			default:
			}
			if !t.next.After(target) {
				due = append(due, t.next)
			}
		}
		for _, a := range c.afters {
			if !a.at.After(target) {
				due = append(due, a.at)
			}
		}
		if len(due) == 0 {
			c.now = target
			c.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool { return due[i].Before(due[j]) })
		at := due[0]
		c.now = at
		var fire []*fakeTicker
		for _, t := range c.tickers {
			if t.next.Equal(at) {
				fire = append(fire, t)
				t.next = t.next.Add(t.period)
			}
		}
		keep := c.afters[:0]
		for _, a := range c.afters {
			if a.at.Equal(at) {
				a.ch <- at
			} else {
				keep = append(keep, a)
			}
		}
		c.afters = keep
		c.mu.Unlock()
		for _, t := range fire {
			select {
			case t.ch <- at:
			case <-t.stop:
			}
		}
	}
}
