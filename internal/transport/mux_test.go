package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// pairedSessions connects a Hub Session and a Worker Session over net.Pipe.
func pairedSessions(t *testing.T, hubOpts, workerOpts SessionOptions) (hub, worker *Session) {
	t.Helper()
	a, b := net.Pipe()
	hub, err := NewSession(a, RoleHub, hubOpts)
	if err != nil {
		t.Fatal(err)
	}
	worker, err = NewSession(b, RoleWorker, workerOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close(); _ = worker.Close() })
	return hub, worker
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionRoundTripAllKinds(t *testing.T) {
	hub, worker := pairedSessions(t, SessionOptions{}, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	// Hub opens op, Worker accepts it.
	op, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.WriteFrame(map[string]any{"type": "op.request", "op": "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := worker.Accept(ctx)
	if err != nil || got.Kind() != KindOp {
		t.Fatalf("worker accept: %v kind %v", err, got)
	}
	if body, err := got.ReadFrame(); err != nil || string(body) != `{"op":"x","type":"op.request"}` {
		t.Fatalf("%s %v", body, err)
	}
	if err := got.WriteFrame(map[string]any{"type": "op.result"}); err != nil {
		t.Fatal(err)
	}
	if body, err := op.ReadFrame(); err != nil || string(body) != `{"type":"op.result"}` {
		t.Fatalf("%s %v", body, err)
	}

	// Worker opens call and ev, Hub accepts both.
	for _, k := range []Kind{KindCall, KindEvents} {
		st, err := worker.OpenStream(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteFrame(map[string]any{"type": "event"}); err != nil {
			t.Fatal(err)
		}
		in, err := hub.Accept(ctx)
		if err != nil || in.Kind() != k {
			t.Fatalf("hub accept kind %v: %v", k, err)
		}
	}
}

func TestSessionCloseEndsAcceptAndPeer(t *testing.T) {
	hub, worker := pairedSessions(t, SessionOptions{}, SessionOptions{})
	if hub.Err() != nil || hub.Role() != RoleHub || worker.Role() != RoleWorker {
		t.Fatal("fresh session state")
	}
	_ = worker.Close()
	select {
	case <-worker.Done():
	default:
		t.Fatal("Done not closed after Close")
	}
	if _, err := worker.Accept(context.Background()); err != ErrSessionClosed {
		t.Fatalf("Accept after Close: %v", err)
	}
	if _, err := worker.OpenCall(context.Background()); err != ErrSessionClosed {
		t.Fatalf("Open after Close: %v", err)
	}
	waitFor(t, "peer session to end", func() bool { return hub.Err() != nil })
}

func TestAcceptHonoursContext(t *testing.T) {
	hub, _ := pairedSessions(t, SessionOptions{}, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := hub.Accept(ctx); err != context.DeadlineExceeded {
		t.Fatalf("got %v", err)
	}
}

func TestNewSessionRejectsUnknownRole(t *testing.T) {
	a, _ := net.Pipe()
	if _, err := NewSession(a, 0, SessionOptions{}); err == nil {
		t.Fatal("role 0 accepted")
	}
}

// A peer that sends nothing for 2 minutes of (fake) time is declared dead and
// the session closes with ErrKeepaliveTimeout.
func TestSessionDiesAfterTwoMinutesOfSilence(t *testing.T) {
	clk := newFakeClock()
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }() // a peer that listens but never answers
	t.Cleanup(func() { _ = b.Close() })
	s, err := NewSession(a, RoleHub, SessionOptions{Keepalive: KeepaliveConfig{Clock: clk}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for clk.waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		clk.Advance(30 * time.Second)
		time.Sleep(20 * time.Millisecond)
		if s.Err() != nil {
			t.Fatalf("dead after %ds: %v", (i+1)*30, s.Err())
		}
	}
	clk.Advance(30 * time.Second)
	select {
	case <-s.Done():
	case <-time.After(testWait):
		t.Fatal("session survived 2 min of silence")
	}
	if s.Err() != ErrKeepaliveTimeout {
		t.Fatalf("cause %v", s.Err())
	}
	if _, err := s.OpenOp(context.Background()); err != ErrKeepaliveTimeout {
		t.Fatalf("open on a dead session: %v", err)
	}
}

// Two live sessions answer each other's pings, so idle time never reaches the
// threshold however long the link is idle.
func TestIdleLinkSurvivesThanksToPings(t *testing.T) {
	clk := newFakeClock()
	opts := SessionOptions{Keepalive: KeepaliveConfig{Clock: clk}}
	hub, worker := pairedSessions(t, opts, opts)
	for clk.waiters() < 2 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 12; i++ { // 6 minutes idle
		clk.Advance(30 * time.Second)
		want := clk.Now()
		waitFor(t, "ping reply on the hub", func() bool { return !hub.conn.lastRead().Before(want) })
		waitFor(t, "ping reply on the worker", func() bool { return !worker.conn.lastRead().Before(want) })
	}
	if hub.Err() != nil || worker.Err() != nil {
		t.Fatalf("idle link died: %v / %v", hub.Err(), worker.Err())
	}
}

// A peer that opens invalid streams and keeps its sending side open must not
// make the session retain more than a bounded number of them: past the limit
// the session ends.
func TestRejectedStreamsStayBounded(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleHub, SessionOptions{})
	for i := 0; i < 2*(maxPendingStreams+maxRejectedStreams); i++ {
		rs, err := raw.ys.OpenStream()
		if err != nil {
			break // the session ended
		}
		if _, err := rs.Write([]byte("zz")); err != nil { // never closed, never reset
			break
		}
	}
	select {
	case <-s.Done():
	case <-time.After(testWait):
		t.Fatalf("session still running with %d streams", s.NumStreams())
	}
	if !errors.Is(s.Err(), ErrTooManyStreams) {
		t.Fatalf("cause %v, want ErrTooManyStreams", s.Err())
	}
	// A few streams that yamux had already queued may be counted on top.
	if n := s.NumStreams(); n > 2*(maxPendingStreams+maxRejectedStreams) {
		t.Fatalf("%d streams retained", n)
	}
}

// Rejected streams that the peer closes are released at once, so honest
// mistakes never add up.
func TestRejectedStreamsClosedByPeerAreReleased(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleHub, SessionOptions{})
	for i := 0; i < 3*maxPendingStreams; i++ {
		rs, err := raw.ys.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = rs.Write([]byte("zz"))
		_ = rs.Close()
		expectEnded(t, rs)
	}
	waitFor(t, "rejected streams released", func() bool { return s.NumStreams() == 0 })
	if s.Err() != nil {
		t.Fatalf("session ended: %v", s.Err())
	}
}

// OpenStream returns when its context ends even if the connection is stalled.
func TestOpenStreamHonoursContextWhenConnectionStalls(t *testing.T) {
	a, b := net.Pipe() // nobody reads b: every write blocks
	defer b.Close()
	s, err := NewSession(a, RoleWorker, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	st, err := s.OpenCall(ctx)
	if err == nil {
		t.Fatalf("OpenCall succeeded: %v", st)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("OpenCall took %v after its deadline", d)
	}
}

// A stream opened after the context ended is not delivered and not leaked.
func TestOpenStreamCancelledDuringPrefixWrite(t *testing.T) {
	s, raw := sessionAndRaw(t, RoleWorker, SessionOptions{})
	_ = raw
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.OpenEvents(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if n := s.NumStreams(); n != 0 {
		t.Fatalf("%d streams after a cancelled open", n)
	}
}

// Release returns only when the stream is really gone: while the peer has not
// completed the close it blocks, and it returns once the peer closed.
func TestStreamReleaseWaitsForThePeerToCompleteTheClose(t *testing.T) {
	hub, worker := pairedSessions(t, SessionOptions{}, SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := worker.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := hub.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		hs.Release()
		close(released)
	}()
	// A handler's late interrupt of its idle read must not end the drain.
	time.AfterFunc(50*time.Millisecond, func() { _ = hs.SetReadDeadline(time.Now()) })
	select {
	case <-released:
		t.Fatal("Release returned while the peer still held its side of the stream")
	case <-time.After(300 * time.Millisecond):
	}
	if n := hub.NumStreams(); n != 1 {
		t.Fatalf("NumStreams = %d while the close is not complete, want 1", n)
	}
	_ = ws.Close()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("Release did not return after the peer closed")
	}
	waitFor(t, "yamux to drop the stream", func() bool { return hub.NumStreams() == 0 })
	hs.Release() // again: harmless
	hs.Reset()
}

// countingBudget is a StreamBudget with room for cap units and cap refusals.
type countingBudget struct {
	mu       sync.Mutex
	cap      int
	used     int
	refused  int
	reserved int // every Reserve that said yes
	denied   int
}

func (b *countingBudget) Reserve() (func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.cap {
		b.denied++
		return nil, false
	}
	b.used++
	b.reserved++
	var once sync.Once
	return func() { once.Do(func() { b.mu.Lock(); b.used--; b.mu.Unlock() }) }, true
}

func (b *countingBudget) Refuse() (func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refused >= b.cap {
		return nil, false
	}
	b.refused++
	var once sync.Once
	return func() { once.Do(func() { b.mu.Lock(); b.refused--; b.mu.Unlock() }) }, true
}

func (b *countingBudget) counts() (used, refused int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.refused
}

// TestStreamBudgetIsConsultedBeforeTheKindIsRead: a stream that never sends its
// prefix holds a unit from the moment yamux accepts it; a stream that does not
// fit is refused (and counted while it drains); a session that overruns the
// refusals is closed; an ordinary stream keeps its unit until its host drops it
// ([Stream.DropReservation]); and a session that ends gives every unit back.
func TestStreamBudgetIsConsultedBeforeTheKindIsRead(t *testing.T) {
	a, b := net.Pipe()
	budget := &countingBudget{cap: 2}
	hub, err := NewSession(a, RoleHub, SessionOptions{Budget: budget, KindTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	peer, err := yamux.Client(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	// Stream 1 is valid, stream 2 is silent.
	ok, err := peer.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ok.Write([]byte("ca")); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.OpenStream(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := hub.Accept(ctx)
	if err != nil || st.Kind() != KindCall {
		t.Fatalf("accept: %v %v", st, err)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				u, r := budget.counts()
				t.Fatalf("timed out waiting for %s (used %d, refused %d)", what, u, r)
			}
		}
	}
	waitFor("two units held", func() bool { u, _ := budget.counts(); return u == 2 })
	// The classified stream still holds its unit until the host drops it.
	if u, _ := budget.counts(); u != 2 {
		t.Fatalf("used = %d", u)
	}
	st.DropReservation()
	st.DropReservation() // idempotent
	waitFor("one unit held", func() bool { u, _ := budget.counts(); return u == 1 })

	// Over the budget: the third and fourth streams are refused and counted.
	if _, err := peer.OpenStream(); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.OpenStream(); err != nil {
		t.Fatal(err)
	}
	waitFor("one unit more", func() bool { u, _ := budget.counts(); return u == 2 })
	waitFor("a refusal counted", func() bool { _, r := budget.counts(); return r >= 1 })

	// Ending the session gives every unit back.
	_ = hub.Close()
	waitFor("everything given back", func() bool { u, r := budget.counts(); return u == 0 && r == 0 })
}

// A peer that keeps opening streams beyond the budget and the refusals is cut
// off: the session ends with ErrTooManyStreams.
func TestStreamBudgetClosesAnAbusiveSession(t *testing.T) {
	a, b := net.Pipe()
	hub, err := NewSession(a, RoleHub, SessionOptions{Budget: &countingBudget{cap: 1}, KindTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	peer, err := yamux.Client(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := peer.OpenStream(); err != nil {
				return
			}
		}
	}()
	select {
	case <-hub.Done():
		if !errors.Is(hub.Err(), ErrTooManyStreams) {
			t.Fatalf("session ended with %v, want ErrTooManyStreams", hub.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an abusive session was not closed")
	}
}
