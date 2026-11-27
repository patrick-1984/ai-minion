package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerKeepaliveBounds(t *testing.T) {
	// A half-open link must be noticed within 45-90 s on the Worker side.
	if WorkerDeadAfter < 45*time.Second || WorkerDeadAfter > 90*time.Second || WorkerKeepaliveInterval >= WorkerDeadAfter/2 {
		t.Fatalf("worker interval %v, dead after %v", WorkerKeepaliveInterval, WorkerDeadAfter)
	}
	if BackoffMax > 60*time.Second {
		t.Fatalf("backoff cap %v", BackoffMax)
	}
}

// A peer that stops answering (the Hub restarted while the device's network
// was down: the connection is half-open) is declared dead after
// WorkerDeadAfter with the Worker's values.
func TestWorkerSessionDiesWithinAMinuteOfSilence(t *testing.T) {
	clk := newFakeClock()
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }() // a blackhole: reads, never answers
	t.Cleanup(func() { _ = b.Close() })
	s, err := NewSession(a, RoleWorker, SessionOptions{Keepalive: KeepaliveConfig{
		Clock: clk, Interval: WorkerKeepaliveInterval, DeadAfter: WorkerDeadAfter,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for clk.waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		clk.Advance(WorkerKeepaliveInterval)
		time.Sleep(20 * time.Millisecond)
		if s.Err() != nil {
			t.Fatalf("dead after %v: %v", time.Duration(i+1)*WorkerKeepaliveInterval, s.Err())
		}
	}
	clk.Advance(WorkerKeepaliveInterval)
	select {
	case <-s.Done():
	case <-time.After(testWait):
		t.Fatal("session survived a minute of silence")
	}
	if !errors.Is(s.Err(), ErrKeepaliveTimeout) {
		t.Fatalf("cause %v", s.Err())
	}
}

// The silence is measured on the clock's own reading, not on a stored wall
// time: lastRead keeps the monotonic reading of the clock.
func TestLastReadKeepsTheMonotonicReading(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ac := &activityConn{Conn: a, clk: SystemClock, base: time.Now()}
	ac.touch()
	got := ac.lastRead()
	if !strings.Contains(got.String(), " m=") {
		t.Fatal("lastRead lost the monotonic reading; a wall-clock step would distort the silence")
	}
	if d := time.Since(got); d < 0 || d > time.Second {
		t.Fatalf("silence %v", d)
	}
}

func TestProbeKeepsALiveLink(t *testing.T) {
	_, worker := pairedSessions(t, SessionOptions{}, SessionOptions{})
	if err := worker.Probe(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("probe of a live link: %v", err)
	}
	if worker.Err() != nil {
		t.Fatalf("session closed: %v", worker.Err())
	}
}

// Probe declares a blackholed link dead at once, without waiting for the
// keepalive's DeadAfter.
func TestProbeDeclaresABlackholedLinkDead(t *testing.T) {
	clk := newFakeClock()
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	t.Cleanup(func() { _ = b.Close() })
	s, err := NewSession(a, RoleWorker, SessionOptions{Keepalive: KeepaliveConfig{Clock: clk}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	res := make(chan error, 1)
	go func() { res <- s.Probe(context.Background(), 0) }()
	for clk.waiters() < 2 { // the keepalive ticker and the probe's timer
		time.Sleep(time.Millisecond)
	}
	clk.Advance(ProbeTimeout)
	select {
	case err := <-res:
		if !errors.Is(err, ErrKeepaliveTimeout) {
			t.Fatalf("probe: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("probe did not end")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("the session is still open after a failed probe")
	}
}

// After a suspend (the wall clock moved on, the monotonic one did not) the
// monitor verifies the link at once: no answer within ProbeTimeout is dead,
// long before DeadAfter of awake time.
func TestRunKeepaliveVerifiesAfterResume(t *testing.T) {
	for _, answer := range []bool{false, true} {
		clk := newFakeClock()
		var wallOffset atomic.Int64
		wall := func() time.Time { return clk.Now().Add(time.Duration(wallOffset.Load())) }
		start := clk.Now()
		var heard atomic.Int64
		heard.Store(int64(start.Sub(start)))
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			result <- RunKeepalive(ctx, KeepaliveConfig{Clock: clk, Interval: 15 * time.Second, DeadAfter: time.Hour, WallNow: wall},
				func() time.Time { return start.Add(time.Duration(heard.Load())) },
				func() error {
					if answer {
						heard.Store(int64(clk.Now().Sub(start)))
						return nil
					}
					select {} // a ping that is never answered
				})
		}()
		for clk.waiters() == 0 {
			time.Sleep(time.Millisecond)
		}
		wallOffset.Store(int64(4 * time.Hour)) // four hours of standby
		clk.Advance(15 * time.Second)
		for clk.waiters() < 2 && !answer {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		clk.Advance(ProbeTimeout)
		select {
		case err := <-result:
			if answer || !errors.Is(err, ErrKeepaliveTimeout) {
				t.Fatalf("answer=%v: %v", answer, err)
			}
		case <-time.After(200 * time.Millisecond):
			if !answer {
				t.Fatal("an unanswered link survived the resume check")
			}
		}
		cancel()
	}
}
