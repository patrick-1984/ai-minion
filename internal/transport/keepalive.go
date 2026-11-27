package transport

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Liveness parameters (AD-3).
const (
	// KeepaliveInterval is how often an idle connection is pinged.
	KeepaliveInterval = 30 * time.Second
	// DeadAfter is the silence after which a connection is declared dead.
	DeadAfter = 2 * time.Minute

	// WorkerKeepaliveInterval and WorkerDeadAfter are the Worker side's
	// tighter values: a Worker (an Android TV in standby, a laptop that
	// sleeps) is the end that loses its network, and it alone can reconnect,
	// so it must notice a half-open connection within about a minute. The Hub
	// keeps the AD-3 values.
	WorkerKeepaliveInterval = 15 * time.Second
	WorkerDeadAfter         = 60 * time.Second

	// ProbeTimeout bounds [Session.Probe] and the check after a resume.
	ProbeTimeout = 15 * time.Second

	// resumeGap is how much more the wall clock must have advanced than the
	// monotonic clock between two ticks for the monitor to conclude that the
	// device was suspended (the monotonic clock stops during a suspend on
	// Linux and Android, so the silence of a long standby is invisible to it).
	resumeGap = 20 * time.Second
)

// ErrKeepaliveTimeout is the cause of a session that was closed because the
// peer sent nothing for DeadAfter.
var ErrKeepaliveTimeout = errors.New("transport: no traffic from the peer, connection is dead")

// KeepaliveConfig tunes RunKeepalive. Zero fields use the AD-3 values.
type KeepaliveConfig struct {
	// Interval is the ping period; zero means KeepaliveInterval.
	Interval time.Duration
	// DeadAfter is the silence that kills the connection; zero means the
	// DeadAfter constant.
	DeadAfter time.Duration
	// Clock is the time source; nil means SystemClock.
	Clock Clock
	// WallNow reads the wall clock without its monotonic reading, to notice a
	// suspend: when the wall clock moved on by more than the monotonic one
	// between two ticks, the link is verified at once with a ping that must be
	// answered within ProbeTimeout. Nil means time.Now().Round(0) with the
	// SystemClock and no resume check with any other clock.
	WallNow func() time.Time

	// pingDone lets same-package tests wait until the monitor can ping again.
	pingDone chan<- struct{}
}

// RunKeepalive watches one connection until ctx ends or the peer is dead.
// Every Interval it calls ping (at most one ping in flight; the reply counts
// as traffic through lastTraffic, the last moment bytes arrived from the
// peer), and when nothing has arrived for DeadAfter it returns
// ErrKeepaliveTimeout. It returns ctx.Err() when ctx
// ends first.
func RunKeepalive(ctx context.Context, cfg KeepaliveConfig, lastTraffic func() time.Time, ping func() error) error {
	interval, dead, clk := cfg.Interval, cfg.DeadAfter, cfg.Clock
	if interval <= 0 {
		interval = KeepaliveInterval
	}
	if dead <= 0 {
		dead = DeadAfter
	}
	if clk == nil {
		clk = SystemClock
	}
	wall := cfg.WallNow
	if wall == nil && clk == SystemClock {
		wall = func() time.Time { return time.Now().Round(0) }
	}
	tk := clk.NewTicker(interval)
	defer tk.Stop()
	start := clk.Now() // the link counts as fresh from here, even if lastTraffic is older
	prevMono := start
	var prevWall time.Time
	if wall != nil {
		prevWall = wall()
	}
	var inflight atomic.Bool
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tk.C():
		}
		now := clk.Now()
		last := lastTraffic()
		if last.Before(start) {
			last = start
		}
		if now.Sub(last) >= dead {
			return ErrKeepaliveTimeout
		}
		if wall != nil {
			w := wall()
			resumed := w.Sub(prevWall)-now.Sub(prevMono) > resumeGap
			prevWall, prevMono = w, now
			if resumed && !verify(ctx, clk, last, lastTraffic, ping) {
				return ErrKeepaliveTimeout
			}
		}
		if inflight.CompareAndSwap(false, true) {
			go func() {
				defer func() {
					inflight.Store(false)
					if cfg.pingDone != nil {
						select {
						case cfg.pingDone <- struct{}{}:
						case <-ctx.Done():
						}
					}
				}()
				_ = ping() // a failed ping shows up as silence
			}()
		}
	}
}

// verify pings once after a resume and reports whether the peer answered (or
// sent anything else) within ProbeTimeout. A context that ends counts as
// alive: the caller returns on its own.
func verify(ctx context.Context, clk Clock, last time.Time, lastTraffic func() time.Time, ping func() error) bool {
	res := make(chan error, 1)
	go func() { res <- ping() }()
	timeout := clk.After(ProbeTimeout)
	for {
		select {
		case err := <-res:
			if err == nil || lastTraffic().After(last) {
				return true
			}
			// A failed ping: wait for the rest of the window for any traffic.
			res = nil
		case <-timeout:
			return lastTraffic().After(last)
		case <-ctx.Done():
			return true
		}
	}
}

// Clock is the time source of the keepalive monitor and the backoff waits. The
// production implementation is SystemClock; tests inject a fake so that
// 30 s and 2 min intervals run instantly.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTicker returns a ticker that fires every d.
	NewTicker(d time.Duration) Ticker
	// After returns a channel that receives once after d.
	After(d time.Duration) <-chan time.Time
}

// Ticker is the part of time.Ticker the transport uses.
type Ticker interface {
	// C returns the channel on which ticks arrive.
	C() <-chan time.Time
	// Stop turns the ticker off.
	Stop()
}

// SystemClock is the Clock backed by the time package.
var SystemClock Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) NewTicker(d time.Duration) Ticker       { return sysTicker{time.NewTicker(d)} }

type sysTicker struct{ t *time.Ticker }

func (s sysTicker) C() <-chan time.Time { return s.t.C }
func (s sysTicker) Stop()               { s.t.Stop() }
