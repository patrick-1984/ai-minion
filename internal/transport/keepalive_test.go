package transport

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeepaliveConstants(t *testing.T) {
	if KeepaliveInterval != 30*time.Second || DeadAfter != 2*time.Minute {
		t.Fatalf("interval %v, dead after %v", KeepaliveInterval, DeadAfter)
	}
}

func TestRunKeepalivePingsEvery30sAndDiesAfter2min(t *testing.T) {
	clk := newFakeClock()
	start := clk.Now()
	pings := make(chan struct{}, 16)
	result := make(chan error, 1)
	go func() {
		result <- RunKeepalive(context.Background(), KeepaliveConfig{Clock: clk},
			func() time.Time { return start },
			func() error { pings <- struct{}{}; return nil })
	}()
	for clk.waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 1; i <= 3; i++ {
		// The loop re-arms its timer after each ping; advance only once it waits,
		// or the advance is lost on a slow machine.
		for clk.waiters() == 0 {
			time.Sleep(time.Millisecond)
		}
		clk.Advance(30 * time.Second)
		select {
		case <-pings:
		case <-time.After(30 * time.Second):
			t.Fatalf("no ping after %d x 30s", i)
		}
		select {
		case err := <-result:
			t.Fatalf("declared dead after %ds: %v", i*30, err)
		default:
		}
	}
	clk.Advance(30 * time.Second)
	select {
	case err := <-result:
		if !errors.Is(err, ErrKeepaliveTimeout) {
			t.Fatalf("got %v, want ErrKeepaliveTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not declared dead after 2 min of silence")
	}
}

func TestRunKeepaliveTrafficKeepsConnectionAlive(t *testing.T) {
	clk := newFakeClock()
	var last atomic.Int64
	last.Store(clk.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	var pings atomic.Int32
	go func() {
		result <- RunKeepalive(ctx, KeepaliveConfig{Clock: clk},
			func() time.Time { return time.Unix(0, last.Load()) },
			func() error { pings.Add(1); return nil })
	}()
	for clk.waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 10; i++ { // 5 minutes, traffic every 30 s
		last.Store(clk.Now().Add(30 * time.Second).UnixNano())
		clk.Advance(30 * time.Second)
	}
	select {
	case err := <-result:
		t.Fatalf("died despite traffic: %v", err)
	default:
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestRunKeepaliveOnePingInFlight(t *testing.T) {
	clk := newFakeClock()
	start := clk.Now()
	release := make(chan struct{})
	var calls atomic.Int32
	started := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = RunKeepalive(ctx, KeepaliveConfig{Clock: clk}, func() time.Time { return start }, func() error {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return nil
		})
	}()
	for clk.waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	clk.Advance(30 * time.Second)
	<-started
	clk.Advance(30 * time.Second) // the first ping is still blocked
	clk.Advance(30 * time.Second)
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d pings in flight, want 1", n)
	}
	close(release)
}

func TestRunKeepaliveCustomThresholds(t *testing.T) {
	clk := newFakeClock()
	start := clk.Now()
	result := make(chan error, 1)
	go func() {
		result <- RunKeepalive(context.Background(),
			KeepaliveConfig{Clock: clk, Interval: time.Second, DeadAfter: 3 * time.Second},
			func() time.Time { return start }, func() error { return nil })
	}()
	for clk.waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	clk.Advance(3 * time.Second)
	if err := <-result; !errors.Is(err, ErrKeepaliveTimeout) {
		t.Fatalf("got %v", err)
	}
}
