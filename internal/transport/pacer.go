package transport

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Bulk pacer of the data legs (BRD NFR-3).
//
// yamux has no stream priorities and one send path per session: every frame
// waits behind what was written before it, down to the socket and the network
// queues. A data leg that writes as fast as the window allows fills those
// queues, and a keystroke echo or an op result then waits behind megabytes of
// file content. The pacer keeps that queue short:
//
//   - fx writes are cut into frames of at most proto.LegChunkBytes and yield
//     after each one (fxconn.go), so frames of op, call and event streams
//     interleave on the send path;
//   - a token bucket shared by the data legs of the session limits their rate
//     to Share (default 80 %) of the measured link rate;
//   - the link rate is measured from the queueing delay of the session's own
//     send path: while a leg writes, the pacer sends a yamux ping every
//     ProbeInterval. The ping waits in the same queue as the data, so its
//     round trip minus the smallest one seen is the queueing delay. While
//     the queue is longer than TargetDelay the link was busy, and the bytes
//     sent in the interval, corrected by the growth of the queue, give the
//     link rate: rate = sent / (interval + delay_now - delay_before). The
//     bucket rate is then set to Share of it, so the queue drains.
//   - with a short queue the rate grows again: it doubles per probe until the
//     first time the queue built up (start of a transfer), then grows by 1/8
//     per probe.
//
// The pacer is per session (one Worker connection) and paces only data legs;
// frame streams are never delayed by it.

// PacerConfig tunes the bulk pacer. The zero value is the production setup.
type PacerConfig struct {
	// Disabled turns the token bucket and the probes off: fx writes are still
	// cut into chunks and yield, but are not rate limited (benchmarks).
	Disabled bool
	// Share is the fraction of the measured link rate the data legs may
	// use; zero means DefaultPacerShare. It is clamped to [0.1, 1].
	Share float64
	// TargetDelay is the queueing delay above which the pacer slows the
	// legs down; zero means DefaultPacerTargetDelay.
	TargetDelay time.Duration
	// ProbeInterval is the period of the delay probes while a leg writes;
	// zero means DefaultPacerProbeInterval.
	ProbeInterval time.Duration
	// InitialRate is the rate a session starts with, in bytes per second;
	// zero means DefaultPacerInitialRate.
	InitialRate float64
	// MinRate is the floor of the rate, in bytes per second; zero means
	// DefaultPacerMinRate.
	MinRate float64
}

// Defaults of the bulk pacer.
const (
	DefaultPacerShare         = 0.8
	DefaultPacerTargetDelay   = 10 * time.Millisecond
	DefaultPacerProbeInterval = 20 * time.Millisecond
	DefaultPacerInitialRate   = 8 << 20
	DefaultPacerMinRate       = 128 << 10

	// pacerIdle ends the probes when no leg has written for this long.
	pacerIdle = time.Second
	// pacerBaseWindow is the age after which the smallest round trip is
	// measured anew (a changed route).
	pacerBaseWindow = 10 * time.Second
	// pacerBurstTime bounds the burst of the bucket, as time at the current
	// rate (at least two chunks).
	pacerBurstTime = 5 * time.Millisecond
)

// errPacerClosed is returned by a wait that ended because the leg or the
// session ended.
var errPacerClosed = errors.New("transport: data leg closed")

// Pacer is the bulk pacer of the data legs of one session.
type Pacer struct {
	cfg  PacerConfig
	ping func() (time.Duration, error)
	done <-chan struct{}
	now  func() time.Time

	mu      sync.Mutex
	rate    float64 // bytes per second
	tokens  float64
	filled  time.Time // last refill of the bucket
	active  time.Time // last write of a leg
	probing bool
	// grown is true once the queue first built up: the rate then grows
	// slowly instead of doubling.
	grown bool

	// Probe state.
	sent      int64 // bytes written by legs since the last probe
	limited   bool  // a writer waited for tokens since the last probe
	base      time.Duration
	baseAt    time.Time
	nextBase  time.Duration
	lastDelay time.Duration
	lastAt    time.Time
	link      float64 // last link rate estimate, bytes per second
}

func newPacer(cfg PacerConfig, ping func() (time.Duration, error), done <-chan struct{}) *Pacer {
	if cfg.Share <= 0 {
		cfg.Share = DefaultPacerShare
	}
	cfg.Share = min(max(cfg.Share, 0.1), 1)
	if cfg.TargetDelay <= 0 {
		cfg.TargetDelay = DefaultPacerTargetDelay
	}
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = DefaultPacerProbeInterval
	}
	if cfg.MinRate <= 0 {
		cfg.MinRate = DefaultPacerMinRate
	}
	if cfg.InitialRate <= 0 {
		cfg.InitialRate = DefaultPacerInitialRate
	}
	cfg.InitialRate = max(cfg.InitialRate, cfg.MinRate)
	return &Pacer{cfg: cfg, ping: ping, done: done, now: time.Now, rate: cfg.InitialRate}
}

// Rate returns the current rate limit of the data legs in bytes per second; 0
// when the pacer is disabled.
func (p *Pacer) Rate() float64 {
	if p == nil || p.cfg.Disabled {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rate
}

// LinkRate returns the last measured link rate in bytes per second; 0 before
// the first measurement.
func (p *Pacer) LinkRate() float64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.link
}

// wait takes n bytes from the bucket and returns when they may be written. It
// returns a timeout error (os.ErrDeadlineExceeded) when the wait would pass
// deadline (zero: none), and errPacerClosed when closed or the session ends.
func (p *Pacer) wait(n int, deadline time.Time, closed <-chan struct{}) error {
	if p == nil || p.cfg.Disabled {
		return nil
	}
	p.mu.Lock()
	now := p.now()
	p.active = now
	if !p.probing {
		p.probing = true
		p.lastAt = time.Time{}
		go p.probe()
	}
	p.refill(now)
	p.tokens -= float64(n)
	if p.tokens >= 0 {
		p.mu.Unlock()
		return nil
	}
	p.limited = true
	d := time.Duration(-p.tokens / p.rate * float64(time.Second))
	p.mu.Unlock()

	if !deadline.IsZero() && now.Add(d).After(deadline) {
		p.giveBack(n)
		d = max(time.Until(deadline), 0)
		if !p.sleep(d, closed) {
			return errPacerClosed
		}
		return os.ErrDeadlineExceeded
	}
	if !p.sleep(d, closed) {
		p.giveBack(n)
		return errPacerClosed
	}
	return nil
}

// sleep waits d; false when closed or the session ended first.
func (p *Pacer) sleep(d time.Duration, closed <-chan struct{}) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-closed:
	case <-p.done:
	}
	return false
}

func (p *Pacer) giveBack(n int) {
	p.mu.Lock()
	p.tokens += float64(n)
	p.mu.Unlock()
}

// refill adds the tokens earned since the last refill; mu is held.
func (p *Pacer) refill(now time.Time) {
	burst := max(p.rate*pacerBurstTime.Seconds(), 2*proto.LegChunkBytes)
	if !p.filled.IsZero() {
		p.tokens += p.rate * now.Sub(p.filled).Seconds()
	}
	p.tokens = min(p.tokens, burst)
	p.filled = now
}

// wrote records n bytes written by a leg.
func (p *Pacer) wrote(n int) {
	if p == nil || p.cfg.Disabled || n <= 0 {
		return
	}
	p.mu.Lock()
	p.sent += int64(n)
	p.mu.Unlock()
}

// probe measures the queueing delay while legs write and adapts the rate. It
// ends when no leg has written for pacerIdle, when a ping fails or when the
// session ends.
func (p *Pacer) probe() {
	t := time.NewTicker(p.cfg.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			p.stopProbing()
			return
		case <-t.C:
		}
		p.mu.Lock()
		idle := p.now().Sub(p.active) > pacerIdle
		if idle {
			p.probing = false
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		rtt, err := p.ping()
		if err != nil {
			p.stopProbing()
			return
		}
		p.sample(p.now(), rtt)
	}
}

func (p *Pacer) stopProbing() {
	p.mu.Lock()
	p.probing = false
	p.mu.Unlock()
}

// sample feeds one round trip, measured at now, into the controller.
func (p *Pacer) sample(now time.Time, rtt time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// The smallest round trip is the empty-queue baseline; it is measured
	// anew every pacerBaseWindow from the smallest of the last window.
	if p.base == 0 || rtt < p.base {
		p.base = rtt
	}
	if p.nextBase == 0 || rtt < p.nextBase {
		p.nextBase = rtt
	}
	if p.baseAt.IsZero() {
		p.baseAt = now
	} else if now.Sub(p.baseAt) > pacerBaseWindow {
		p.base, p.nextBase, p.baseAt = p.nextBase, 0, now
	}
	delay := rtt - p.base

	sent, limited := p.sent, p.limited
	p.sent, p.limited = 0, false
	prevDelay, prevAt := p.lastDelay, p.lastAt
	p.lastDelay, p.lastAt = delay, now
	if prevAt.IsZero() {
		return // the first sample of a busy period only sets the start
	}
	dt := now.Sub(prevAt)
	if dt <= 0 {
		return
	}

	if delay > p.cfg.TargetDelay {
		p.grown = true
		// The queue is long: the link was busy. Estimate its rate from the
		// bytes sent and the growth of the queue, and leave Share of it.
		span := dt + delay - prevDelay
		var est float64
		if prevDelay > p.cfg.TargetDelay/2 && span > dt/4 {
			est = float64(sent) / span.Seconds()
		}
		next := p.rate * 0.7
		if est > 0 {
			p.link = est
			next = min(next, p.cfg.Share*est)
			if delay > 2*p.cfg.TargetDelay {
				next = min(next, p.cfg.Share*est*0.8)
			}
		}
		p.rate = max(next, p.cfg.MinRate)
		p.tokens = min(p.tokens, 0)
		return
	}
	if delay > p.cfg.TargetDelay/2 || !limited {
		return // hold: the queue is building, or the legs did not use the rate
	}
	// A short queue at the rate the legs used: the link carries at least
	// that much.
	p.link = max(p.link, float64(sent)/dt.Seconds())
	if !p.grown {
		p.rate *= 2
	} else {
		p.rate *= 1.125
	}
	if p.link > 0 {
		// Never more than a little above what the link carried.
		p.rate = min(p.rate, 1.5*p.link)
	}
	p.rate = max(p.rate, p.cfg.MinRate)
}
