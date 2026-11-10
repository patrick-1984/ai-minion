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
	tk := clk.NewTicker(interval)
	defer tk.Stop()
	start := clk.Now() // the link counts as fresh from here, even if lastTraffic is older
	var inflight atomic.Bool
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tk.C():
		}
		last := lastTraffic()
		if last.Before(start) {
			last = start
		}
		if clk.Now().Sub(last) >= dead {
			return ErrKeepaliveTimeout
		}
		if inflight.CompareAndSwap(false, true) {
			go func() {
				defer inflight.Store(false)
				_ = ping() // a failed ping shows up as silence
			}()
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
