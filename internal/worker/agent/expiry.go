package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// ExpiryTick is how often a connection re-reads both clocks besides the timer
// set to the deadline (the keepalive rate, design hub-identity-v2 section 11.3).
const ExpiryTick = 30 * time.Second

// ErrHubCertExpired is the cause of a connection that the Worker ended because
// the Hub certificate reached its not_after (wall clock or monotonic deadline).
// It never touches the enrollment or the pin.
var ErrHubCertExpired = errors.New("agent: the Hub certificate expired; the connection was closed (the pairing is kept)")

// expiryGuard enforces the end of one connection (section 11.3): the session is
// over when the wall clock is at or past not_after OR the monotonic clock is at
// or past the deadline fixed at the window check. Both clocks are read on every
// check. The first check that finds the end closes the session.
type expiryGuard struct {
	exp transport.Expiry
	clk transport.WallMono
	log *slog.Logger

	mu       sync.Mutex
	fired    bool
	onExpire func()
}

type guardKey struct{}

func withGuard(ctx context.Context, g *expiryGuard) context.Context {
	return context.WithValue(ctx, guardKey{}, g)
}

// guardFrom returns the expiry guard of a link context, or nil when the link
// has no expiry (a test double that does not model the certificate).
func guardFrom(ctx context.Context) *expiryGuard {
	g, _ := ctx.Value(guardKey{}).(*expiryGuard)
	return g
}

// check reports whether the connection is over and, the first time, closes it.
func (g *expiryGuard) check() bool {
	if g == nil {
		return false
	}
	if !g.exp.Expired(g.clk) {
		g.mu.Lock()
		fired := g.fired
		g.mu.Unlock()
		return fired
	}
	g.mu.Lock()
	first := !g.fired
	g.fired = true
	f := g.onExpire
	g.mu.Unlock()
	if first {
		g.log.Info("the Hub certificate reached its not_after; the connection is closed and the pairing is kept")
		if f != nil {
			f()
		}
	}
	return true
}

// errEnrollExpired is the failure of an enrollment whose Hub certificate
// reached its not_after before the step that needed it was done.
var errEnrollExpired = errors.New("the Hub certificate expired during the enrollment; nothing was stored")

// enrollGuard guards the channel of a pairing or invitation enrollment until it
// completes (section 12): both clocks are read at every check, and at the end of
// the certificate the channel is closed, so that a blocked read or write ends.
// A nil Expiry (a test double) is not enforced. stop ends the timer.
func enrollGuard(exp *transport.Expiry, clk transport.WallMono, closeCh func()) (g *expiryGuard, stop func()) {
	if exp == nil {
		return nil, func() {}
	}
	if clk == nil {
		clk = transport.SystemWallMono
	}
	g = &expiryGuard{exp: *exp, clk: clk, log: slog.New(slog.NewTextHandler(io.Discard, nil)), onExpire: closeCh}
	ctx, cancel := context.WithCancel(context.Background())
	go g.watch(ctx, transport.SystemClock)
	return g, cancel
}

// Expired reports whether the connection of l has reached the end of the Hub
// certificate (and closes it when it has). A link without an expiry never
// expires.
func (l *Link) Expired() bool { return guardFrom(l.ctx).check() }

// guardedSession checks the expiry when a stream is accepted or opened, in
// either direction.
type guardedSession struct {
	Session
	g *expiryGuard
}

func (s guardedSession) Accept(ctx context.Context) (*transport.Stream, error) {
	st, err := s.Session.Accept(ctx)
	if err != nil {
		return nil, err
	}
	if s.g.check() {
		st.Reset()
		return nil, ErrHubCertExpired
	}
	return st, nil
}

func (s guardedSession) OpenCall(ctx context.Context) (*transport.Stream, error) {
	if s.g.check() {
		return nil, ErrHubCertExpired
	}
	return s.Session.OpenCall(ctx)
}

func (s guardedSession) OpenEvents(ctx context.Context) (*transport.Stream, error) {
	if s.g.check() {
		return nil, ErrHubCertExpired
	}
	return s.Session.OpenEvents(ctx)
}

// watch runs the deadline timer and the periodic check until ctx ends. The
// timer is set from the monotonic reading, the tick re-reads the wall clock too,
// so that a wall clock that jumped (or a sleep) is noticed at the next tick.
func (g *expiryGuard) watch(ctx context.Context, clk transport.Clock) {
	d := g.exp.DeadlineMono - g.clk.Mono()
	deadline := clk.After(max(d, 0))
	tick := clk.NewTicker(ExpiryTick)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			deadline = nil
			if g.check() {
				return
			}
		case <-tick.C():
			if g.check() {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
