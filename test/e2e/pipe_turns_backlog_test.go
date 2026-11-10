//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// These tests run the Worker's real turn emitter against the Hub's real turn
// server (workers.Pipe.ServeTurns over a real store and its real quotas), joined
// by an in-memory stream, with one simulated clock for both. They show the
// availability of a healthy Worker whose outbox holds a backlog: the Hub tells
// it to wait (busy) and the Worker, which obeys, keeps its stream and its
// connection and delivers everything once capacity is back. The unit tests of
// both sides check the pieces; the failure these tests guard against
// (a backlog above about 50 turns, or a storage-quota period, ending in a reset
// of the turn stream, a reset again after the reopen and the closing of the
// Worker's whole connection) is in how they interact.

// simClock is a manual clock shared by the Hub and the Worker.
type simClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []simTimer
}

type simTimer struct {
	at time.Time
	ch chan time.Time
}

func newSimClock() *simClock { return &simClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)} }

func (c *simClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *simClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, simTimer{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *simClock) NewTicker(time.Duration) transport.Ticker { panic("unused") }

func (c *simClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
		} else {
			keep = append(keep, t)
		}
	}
	c.timers = keep
}

// memEnd is one end of an in-memory stream. It is a workers.FrameStream (the
// Hub's end) and a wagent.EventStream (the Worker's end).
type memEnd struct {
	in, out chan json.RawMessage
	done    chan struct{}
	once    *sync.Once
	intr    chan struct{}
	intrOnc sync.Once
	resets  *atomic.Int32 // Reset calls on this end
}

func newPair() (worker, hub *memEnd) {
	a, b := make(chan json.RawMessage, 4096), make(chan json.RawMessage, 4096)
	done, once := make(chan struct{}), new(sync.Once)
	worker = &memEnd{in: b, out: a, done: done, once: once, intr: make(chan struct{}), resets: new(atomic.Int32)}
	hub = &memEnd{in: a, out: b, done: done, once: once, intr: make(chan struct{}), resets: new(atomic.Int32)}
	return worker, hub
}

func (e *memEnd) ReadFrame() (json.RawMessage, error) {
	select {
	case f := <-e.in:
		return f, nil
	case <-e.done:
		return nil, io.EOF
	case <-e.intr:
		return nil, io.ErrClosedPipe
	}
}

func (e *memEnd) WriteFrame(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case <-e.done:
		return io.ErrClosedPipe
	default:
	}
	select {
	case e.out <- b:
		return nil
	case <-e.done:
		return io.ErrClosedPipe
	}
}

func (e *memEnd) Close() error                     { e.once.Do(func() { close(e.done) }); return nil }
func (e *memEnd) Reset()                           { e.resets.Add(1); e.once.Do(func() { close(e.done) }) }
func (e *memEnd) SetWriteDeadline(time.Time) error { return nil }
func (e *memEnd) SetReadDeadline(t time.Time) error {
	if !t.After(time.Now()) {
		e.intrOnc.Do(func() { close(e.intr) })
	}
	return nil
}

// hubTap watches the Hub's end of the turn channel: what the Worker sends and
// how it relates to the holds the Hub announced.
type hubTap struct {
	*memEnd
	clk *simClock

	mu          sync.Mutex
	holdEnd     time.Time           // the end of the latest hold a busy answer announced
	outstanding map[string]struct{} // sent, no answer yet
	maxOut      int
	frames      int // turn frames
	early       int // turn frames that arrived while a hold was on
	busy        int
	stored      int
	storedIDs   map[string]int
}

func (t *hubTap) ReadFrame() (json.RawMessage, error) {
	f, err := t.memEnd.ReadFrame()
	if err != nil {
		return f, err
	}
	var h struct {
		Type   string `json:"type"`
		TurnID string `json:"turn_id"`
	}
	if json.Unmarshal(f, &h) == nil && h.Type == proto.TypeTurn {
		t.mu.Lock()
		t.frames++
		if t.clk.Now().Before(t.holdEnd) {
			t.early++
		}
		t.outstanding[h.TurnID] = struct{}{}
		t.maxOut = max(t.maxOut, len(t.outstanding))
		t.mu.Unlock()
	}
	return f, nil
}

func (t *hubTap) WriteFrame(v any) error {
	if a, ok := v.(workers.TurnAckFrame); ok {
		t.mu.Lock()
		delete(t.outstanding, a.TurnID)
		switch a.Status {
		case pipe.AckBusy:
			t.busy++
			if end := t.clk.Now().Add(time.Duration(a.RetryAfterMS) * time.Millisecond); end.After(t.holdEnd) {
				t.holdEnd = end
			}
		case pipe.AckStored:
			t.stored++
			t.storedIDs[a.TurnID]++
		}
		t.mu.Unlock()
	}
	return t.memEnd.WriteFrame(v)
}

func (t *hubTap) snapshot() (frames, early, busy, stored, maxOut int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frames, t.early, t.busy, t.stored, t.maxOut
}

// workerTap watches the Worker's end: the turns it has sent that no answer has
// reached it yet, which is what the Worker's window bounds.
type workerTap struct {
	*memEnd
	mu          sync.Mutex
	outstanding map[string]struct{}
	maxOut      int
}

func (w *workerTap) WriteFrame(v any) error {
	b, _ := json.Marshal(v)
	var h struct {
		Type   string `json:"type"`
		TurnID string `json:"turn_id"`
	}
	if json.Unmarshal(b, &h) == nil && h.Type == proto.TypeTurn {
		w.mu.Lock()
		w.outstanding[h.TurnID] = struct{}{}
		w.maxOut = max(w.maxOut, len(w.outstanding))
		w.mu.Unlock()
	}
	return w.memEnd.WriteFrame(v)
}

func (w *workerTap) ReadFrame() (json.RawMessage, error) {
	f, err := w.memEnd.ReadFrame()
	if err == nil {
		var h struct {
			Type   string `json:"type"`
			TurnID string `json:"turn_id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(f, &h) == nil && h.Type == proto.TypeTurnAck && h.Status != string(pipe.AckBusy) {
			w.mu.Lock()
			delete(w.outstanding, h.TurnID)
			w.mu.Unlock()
		} else if h.Type == proto.TypeTurnAck {
			w.mu.Lock()
			delete(w.outstanding, h.TurnID) // busy: the turn is not in flight any more
			w.mu.Unlock()
		}
	}
	return f, err
}

// The fakes of the parts of the Hub that the turn server does not exercise.
type allowAll struct{}

func (allowAll) Decide(context.Context, authz.Principal, ops.ActionSet, authz.Resource) error {
	return nil
}

type fixedTrust struct{ owner string }

func (fixedTrust) Trusted(context.Context, string, string, string) (bool, error) { return true, nil }
func (f fixedTrust) Owner(context.Context, string) (string, error)               { return f.owner, nil }

type noRouter struct{}

func (noRouter) Call(context.Context, workers.CallRequest) (any, error) {
	return nil, fmt.Errorf("the turn tests send no messages")
}

type allShared struct{}

func (allShared) Shared(context.Context, string) (bool, error) { return true, nil }

type turnRig struct {
	t       *testing.T
	clk     *simClock
	ps      *store.PipeStore
	pipe    *workers.Pipe
	em      *wagent.TurnEmitter
	out     wagent.TurnOutbox
	peer    pipe.Peer
	aborted atomic.Int32

	mu    sync.Mutex
	taps  []*hubTap
	wtaps []*workerTap
}

const rigWorker = "00000000-0000-7000-8000-0000000000a1"

func newTurnRig(t *testing.T, q workers.TurnQuotas) *turnRig {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &turnRig{t: t, clk: newSimClock(), ps: s.Pipe(), peer: pipe.Peer{WorkerID: rigWorker, SessionName: "minion-1"}}
	p, err := workers.NewPipe(workers.PipeConfig{
		Store: r.ps, Authz: allowAll{}, Router: noRouter{}, Trust: fixedTrust{owner: "owner-1"}, Now: r.clk.Now,
		Quotas: q, Logger: slog.New(slog.DiscardHandler),
		Abort: func(id string) {
			if id == rigWorker {
				r.aborted.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.pipe = p
	r.out = wagent.NewMemOutbox(0)
	em, err := wagent.NewTurnEmitter(wagent.TurnConfig{
		Shares: allShared{}, Outbox: r.out, Clock: r.clk,
		AckTimeout: 2 * time.Hour, // the tests are about busy, not about lost acks
	})
	if err != nil {
		t.Fatal(err)
	}
	r.em = em
	return r
}

// start runs the emitter on turn channels that the Hub's ServeTurns serves.
func (r *turnRig) start() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	open := func(context.Context) (wagent.EventStream, error) {
		w, h := newPair()
		tap := &hubTap{memEnd: h, clk: r.clk, outstanding: map[string]struct{}{}, storedIDs: map[string]int{}}
		r.mu.Lock()
		wt := &workerTap{memEnd: w, outstanding: map[string]struct{}{}}
		r.taps = append(r.taps, tap)
		r.wtaps = append(r.wtaps, wt)
		r.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The Hub's ingest takes turns.open and hands the stream over.
			if _, err := tap.ReadFrame(); err != nil {
				return
			}
			r.pipe.ServeTurns(ctx, rigWorker, tap)
		}()
		return wt, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); r.em.ServeStreams(ctx, open) }()
	return func() { cancel(); <-done; wg.Wait() }
}

// seed stores n deliveries of the Hub (as pipe_send does) and emits the reply
// of each on the Worker.
func (r *turnRig) seed(n int, at time.Time) []int64 {
	r.t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		m, err := r.ps.AppendMessage(context.Background(), pipe.Message{Peer: r.peer, Text: fmt.Sprintf("message %d", i), At: at})
		if err != nil {
			r.t.Fatal(err)
		}
		ids = append(ids, m.ID)
		if _, err := r.em.Emit(wagent.PendingTurn{MessageID: m.ID, Session: r.peer.SessionName, Text: fmt.Sprintf("reply %d", i)}); err != nil {
			r.t.Fatal(err)
		}
	}
	return ids
}

func (r *turnRig) pending() int {
	p, err := r.out.Pending()
	if err != nil {
		r.t.Fatal(err)
	}
	return len(p)
}

// run steps the simulated clock until cond holds or the budget of simulated
// time is used. Real time per step lets the goroutines of both sides settle.
func (r *turnRig) run(step time.Duration, budget time.Duration, cond func() bool) bool {
	for elapsed := time.Duration(0); elapsed <= budget; elapsed += step {
		if cond() {
			return true
		}
		r.clk.Advance(step)
		time.Sleep(3 * time.Millisecond)
	}
	return cond()
}

func (r *turnRig) totals() (frames, early, busy, stored, maxOut int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.taps {
		f, e, b, s, _ := t.snapshot()
		frames, early, busy, stored = frames+f, early+e, busy+b, stored+s
	}
	for _, w := range r.wtaps {
		w.mu.Lock()
		maxOut = max(maxOut, w.maxOut)
		w.mu.Unlock()
	}
	return
}

func (r *turnRig) storedTurns() int {
	turns, err := r.ps.Turns(context.Background(), r.peer, "", 100000)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(turns)
}

// A backlog of 70 turns is above what the Worker's rate limit takes at once
// (burst 20, 5 per second): the Hub answers busy, and the Worker must wait, keep
// its stream and its connection and deliver all 70 exactly once.
func TestBacklogAboveTheRateLimitKeepsTheStreamAndTheConnection(t *testing.T) {
	// A slow Worker rate (burst 5, one turn per 20 seconds) so that 70 turns are
	// a backlog whatever the speed of the machine: the Worker cannot send faster
	// than the simulated clock refills the bucket.
	r := newTurnRig(t, workers.TurnQuotas{WorkerTurnRate: 0.05, WorkerTurnBurst: 5})
	r.seed(70, r.clk.Now())
	stop := r.start()
	defer stop()

	// The first busy answers come within the first steps; reset the stream of
	// the Worker once in the middle of the wait, as a reconnect would: the hold
	// must outlive it.
	if !r.run(time.Second, time.Minute, func() bool { _, _, busy, _, _ := r.totals(); return busy > 0 }) {
		t.Fatal("the Hub never answered busy: the backlog did not exceed its rate limit")
	}
	r.mu.Lock()
	first := r.taps[0]
	r.mu.Unlock()
	first.Close()

	if !r.run(time.Second, 90*time.Minute, func() bool { return r.pending() == 0 }) {
		t.Fatalf("%d turns still pending after 90 simulated minutes", r.pending())
	}
	frames, early, busy, stored, maxOut := r.totals()
	t.Logf("frames %d, early %d, busy %d, stored acks %d, most outstanding %d", frames, early, busy, stored, maxOut)

	if n := r.aborted.Load(); n != 0 {
		t.Fatalf("the Hub closed the connection of a compliant Worker %d times", n)
	}
	r.mu.Lock()
	for i, tap := range r.taps {
		// Stream 0 is the one the test closed itself: the Hub's write to it
		// fails and it resets that stream, which is not a strike.
		if n := tap.resets.Load(); n != 0 && i != 0 {
			t.Errorf("the Hub reset turn stream %d of a compliant Worker", i)
		}
	}
	nstreams := len(r.taps)
	r.mu.Unlock()
	if nstreams < 2 {
		t.Errorf("only %d stream opened: the test did not reopen the channel", nstreams)
	}
	if maxOut > wagent.DefaultMaxInFlight {
		t.Errorf("%d turns were outstanding at once, more than the window of %d", maxOut, wagent.DefaultMaxInFlight)
	}
	// A frame that arrives before the end of the hold of an earlier busy answer
	// is one that was already in flight, or one probe too many while the window
	// widens; the Hub tolerates that (it counts strikes per hold, not per
	// answer) and the Worker keeps it small: the whole of it stays far below
	// what a Worker that ignores the holds sends (about one frame per turn per
	// reopen).
	if frames > 4*70 {
		t.Errorf("%d turn frames for 70 turns (%d early): the Worker keeps hammering a Hub that told it to wait", frames, early)
	}
	if got := r.storedTurns(); got != 70 {
		t.Fatalf("%d turns stored, want 70 (none lost, none twice)", got)
	}
	// Every turn was acknowledged stored; none was stored by a duplicate frame.
	r.mu.Lock()
	seen := map[string]int{}
	for _, tap := range r.taps {
		tap.mu.Lock()
		for id, n := range tap.storedIDs {
			seen[id] += n
		}
		tap.mu.Unlock()
	}
	r.mu.Unlock()
	if len(seen) != 70 {
		t.Errorf("%d distinct turns acknowledged stored, want 70", len(seen))
	}
}

// A stored-volume quota that is full for a long period (here 5 simulated
// minutes, many busy holds of a minute) with 40 turns waiting: the Worker stays
// connected the whole time, sends at most a window per hold, keeps its backlog
// bounded and intact, and recovers when the Hub has room again.
func TestStoredQuotaPeriodHoldsTheChannelWithoutLosingTheConnection(t *testing.T) {
	r := newTurnRig(t, workers.TurnQuotas{Stored: store.TurnLimits{WorkerTurns: 10}})
	// The deliveries are dated far ahead so that the retention prune below,
	// which frees the quota, removes the stored replies and not the deliveries.
	r.seed(50, r.clk.Now().Add(30*24*time.Hour))
	stop := r.start()
	defer stop()

	if !r.run(time.Second, 10*time.Minute, func() bool { return r.storedTurns() >= 10 }) {
		t.Fatalf("only %d turns stored", r.storedTurns())
	}
	// The quota is full now. Let 5 simulated minutes pass.
	r.run(time.Second, 5*time.Minute, func() bool { return false })
	if got := r.storedTurns(); got != 10 {
		t.Fatalf("%d turns stored while the quota is 10", got)
	}
	if got := r.pending(); got != 40 {
		t.Fatalf("%d turns pending during the quota period, want the 40 that did not fit (bounded, none dropped)", got)
	}
	frames, early, busy, _, maxOut := r.totals()
	t.Logf("during the period: frames %d, early %d, busy %d, most outstanding %d", frames, early, busy, maxOut)
	if n := r.aborted.Load(); n != 0 {
		t.Fatalf("the Hub closed the connection of a compliant Worker %d times", n)
	}
	r.mu.Lock()
	nstreams := len(r.taps)
	for i, tap := range r.taps {
		if n := tap.resets.Load(); n != 0 {
			t.Errorf("the Hub reset turn stream %d of a compliant Worker", i)
		}
	}
	r.mu.Unlock()
	if nstreams != 1 {
		t.Errorf("%d streams were opened; a Worker that obeys the holds keeps its stream", nstreams)
	}
	// Over 5 minutes and holds of one minute the Worker may try again about
	// once per hold, a window at a time; never every turn at once.
	if frames > 10+8*(1+5+2) {
		t.Errorf("%d turn frames in the period: the Worker keeps hammering a Hub that told it to wait", frames)
	}

	// Room again: the retention prune removes the stored replies.
	if _, _, err := r.ps.DeleteBefore(context.Background(), r.clk.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !r.run(time.Second, 30*time.Minute, func() bool { return r.pending() < 40 }) {
		t.Fatal("the Worker did not deliver again once the Hub had room")
	}
	if n := r.aborted.Load(); n != 0 {
		t.Fatalf("the Hub closed the connection %d times", n)
	}
}
