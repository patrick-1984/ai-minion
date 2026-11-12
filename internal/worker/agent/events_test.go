package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// manualClock is a transport.Clock whose After timers fire only when the test
// advances it. waiters tells how many timers are pending, so a test can wait
// until the code under test sleeps before it moves time.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	at time.Time
	ch chan time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t.ch
}

func (c *manualClock) NewTicker(time.Duration) transport.Ticker { panic("unused") }

// waiters returns the number of timers that have not fired.
func (c *manualClock) waiters() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.timers) }

// Advance moves time forward and fires the timers that fall due.
func (c *manualClock) Advance(d time.Duration) {
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

// dueIn reports whether a timer is pending that falls due exactly d from now.
func (c *manualClock) dueIn(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := c.now.Add(d)
	for _, t := range c.timers {
		if t.at.Equal(want) {
			return true
		}
	}
	return false
}

// tick lets the publisher take one look: it waits for any pending timer and
// advances by d, which fires every timer due by then (the observation timer, an
// ack timer, a split flush...). Tests that move time across several timers (an
// 11 second tick over a 10 second ack timer, a 5 and a 6 second split) rely on
// this, so it must not look at the length of a timer.
func (c *manualClock) tick(t testing.TB, d time.Duration) {
	t.Helper()
	waitFor2(t, func() bool { return c.waiters() > 0 })
	c.Advance(d)
}

// tickDue is tick for a timer of exactly d: it waits until the code under test
// sleeps on a timer that falls due exactly d from now, and only then advances.
// A stream the Hub ended leaves its observation timer behind: advancing on that
// one before the reopen delay is registered would skip the delay for good, so a
// test that needs the reopen delay waits for that delay with tickDue.
func (c *manualClock) tickDue(t testing.TB, d time.Duration) {
	t.Helper()
	waitFor2(t, func() bool { return c.dueIn(d) })
	c.Advance(d)
}

func waitFor2(t testing.TB, cond func() bool) {
	t.Helper()
	// A failure-only deadline: a passing wait returns as soon as cond holds.
	// It is generous because race-instrumented CI runners are several times
	// slower than a developer machine and a 5 s bound flaked there.
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

// scriptObserver returns a view the test sets.
type scriptObserver struct {
	mu    sync.Mutex
	view  View
	err   error
	calls int
}

func (o *scriptObserver) set(states ...SessionState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.view = View{}
	for _, s := range states {
		o.view[s.Name] = s
	}
}

func (o *scriptObserver) fail(err error) { o.mu.Lock(); o.err = err; o.mu.Unlock() }

func (o *scriptObserver) Observe(context.Context) (View, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.err != nil {
		return nil, o.err
	}
	v := View{}
	for k, s := range o.view {
		v[k] = s
	}
	return v, nil
}

func (o *scriptObserver) count() int { o.mu.Lock(); defer o.mu.Unlock(); return o.calls }

// fakeEvStream is an EventStream that queues the frames written to it and
// lets the test end it from the Hub side.
type fakeEvStream struct {
	frames chan json.RawMessage
	in     chan json.RawMessage
	hubEnd chan struct{}
	once   sync.Once

	mu       sync.Mutex
	closed   bool
	reset    bool
	writeErr error
}

func newFakeEvStream() *fakeEvStream {
	return &fakeEvStream{
		frames: make(chan json.RawMessage, 256), in: make(chan json.RawMessage, 8), hubEnd: make(chan struct{}),
	}
}

func (s *fakeEvStream) ReadFrame() (json.RawMessage, error) {
	select {
	case f := <-s.in:
		return f, nil
	case <-s.hubEnd:
		return nil, io.EOF
	}
}

func (s *fakeEvStream) WriteFrame(v any) error {
	s.mu.Lock()
	err := s.writeErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := proto.FrameType(b); err != nil {
		return err
	}
	s.frames <- b
	return nil
}

func (s *fakeEvStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.endHub()
	return nil
}
func (s *fakeEvStream) Reset()                           { s.mu.Lock(); s.reset = true; s.mu.Unlock(); s.endHub() }
func (s *fakeEvStream) SetWriteDeadline(time.Time) error { return nil }
func (s *fakeEvStream) endHub()                          { s.once.Do(func() { close(s.hubEnd) }) }

func (s *fakeEvStream) failWrites(err error) { s.mu.Lock(); s.writeErr = err; s.mu.Unlock() }

func (s *fakeEvStream) wasClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *fakeEvStream) wasReset() bool  { s.mu.Lock(); defer s.mu.Unlock(); return s.reset }

// next reads the next written frame, failing after a timeout.
func (s *fakeEvStream) next(t testing.TB) json.RawMessage {
	t.Helper()
	select {
	case f := <-s.frames:
		return f
	case <-time.After(30 * time.Second): // failure-only bound, see waitFor2
		t.Fatal("no frame within 30 s")
		return nil
	}
}

func (s *fakeEvStream) none(t testing.TB) {
	t.Helper()
	select {
	case f := <-s.frames:
		t.Fatalf("unexpected frame %s", f)
	case <-time.After(20 * time.Millisecond):
	}
}

func (s *fakeEvStream) snapshot(t testing.TB) SnapshotFrame {
	t.Helper()
	f := s.next(t)
	var snap SnapshotFrame
	if err := json.Unmarshal(f, &snap); err != nil || snap.Type != proto.TypeEventsSnap {
		t.Fatalf("frame %s is not events.snapshot (%v)", f, err)
	}
	return snap
}

func (s *fakeEvStream) event(t testing.TB) EventFrame {
	t.Helper()
	f := s.next(t)
	var ev EventFrame
	if err := json.Unmarshal(f, &ev); err != nil || ev.Type != proto.TypeEvent {
		t.Fatalf("frame %s is not an event (%v)", f, err)
	}
	return ev
}

// evHarness runs a publisher over fake streams.
type evHarness struct {
	t       *testing.T
	obs     *scriptObserver
	clk     *manualClock
	pub     *EventPublisher
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	streams []*fakeEvStream
	opens   chan *fakeEvStream
	openErr error
	acks    chan string
}

func startEvents(t *testing.T, states ...SessionState) *evHarness {
	t.Helper()
	h := &evHarness{
		t: t, obs: &scriptObserver{}, clk: newManualClock(), opens: make(chan *fakeEvStream, 16),
		done: make(chan struct{}), acks: make(chan string, 8),
	}
	h.obs.set(states...)
	pub, err := NewEventPublisher(EventConfig{
		Observer: h.obs, Interval: time.Second, Clock: h.clk, ReopenDelay: 5 * time.Second,
		OnFrame: func(ty string, _ json.RawMessage) { h.acks <- ty },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.pub = pub
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { cancel(); <-h.done })
	open := func(context.Context) (EventStream, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.openErr != nil {
			return nil, h.openErr
		}
		st := newFakeEvStream()
		h.streams = append(h.streams, st)
		h.opens <- st
		return st, nil
	}
	go func() { pub.serve(ctx, open); close(h.done) }()
	return h
}

func (h *evHarness) stream() *fakeEvStream {
	h.t.Helper()
	select {
	case st := <-h.opens:
		return st
	case <-time.After(5 * time.Second):
		h.t.Fatal("no event stream was opened")
		return nil
	}
}

func agentState(name, state, hash string) SessionState {
	return SessionState{Name: name, Kind: "agent:claude", State: state, ScreenHash: hash}
}

func TestNewEventPublisherNeedsAnObserver(t *testing.T) {
	if _, err := NewEventPublisher(EventConfig{}); err == nil {
		t.Fatal("a publisher without an observer was accepted")
	}
}

// AC 2: one ev stream per connect, events.snapshot first, then events whose
// seq grows by one.
func TestEventsStartWithASnapshotThenSequencedEvents(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"), agentState("b", "busy", "h2"))
	st := h.stream()
	snap := st.snapshot(t)
	if snap.Seq != 0 || len(snap.Sessions) != 2 || snap.Sessions[0].Name != "a" || snap.Sessions[1].Name != "b" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Sessions[0].Kind != "agent:claude" || snap.Sessions[1].State != "busy" {
		t.Fatalf("snapshot sessions = %+v", snap.Sessions)
	}

	h.clk.tick(t, time.Second)
	st.none(t) // nothing changed, nothing is sent

	h.obs.set(agentState("a", "busy", "h1"), agentState("b", "busy", "h3"), agentState("c", "stable", "h4"))
	h.clk.tick(t, time.Second)
	var got []EventFrame
	for i := 0; i < 3; i++ {
		got = append(got, st.event(t))
	}
	want := []struct {
		seq   uint64
		event string
		sess  string
	}{{1, EventStateChanged, "a"}, {2, EventScreenChanged, "b"}, {3, EventSessionAdded, "c"}}
	for i, w := range want {
		if got[i].Seq != w.seq || got[i].Event != w.event || got[i].Session != w.sess {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], w)
		}
	}
	if got[0].State != "busy" || got[1].ScreenHash != "h3" || got[2].Kind != "agent:claude" || got[2].ScreenHash != "h4" {
		t.Fatalf("event payloads = %+v", got)
	}

	h.obs.set(agentState("a", "busy", "h1"), agentState("c", "stable", "h4"))
	h.clk.tick(t, time.Second)
	if ev := st.event(t); ev.Seq != 4 || ev.Event != EventSessionRemoved || ev.Session != "b" || ev.State != "" || ev.ScreenHash != "" {
		t.Fatalf("removed event = %+v", ev)
	}
}

func TestSnapshotAndEventsCarryNoScreenContent(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	st := h.stream()
	raw := st.next(t)
	for _, banned := range []string{`"lines"`, `"spans"`, `"cursor"`, `"text"`, `"screen":`} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("snapshot %s contains %s", raw, banned)
		}
	}
	h.obs.set(agentState("a", "stable", "h9"))
	h.clk.tick(t, time.Second)
	raw = st.next(t)
	if strings.Contains(string(raw), `"lines"`) || !strings.Contains(string(raw), `"screen_hash":"h9"`) {
		t.Fatalf("screen_changed = %s", raw)
	}
}

// AC 2: each (re)connect gets a snapshot and seq starts again.
func TestEachConnectGetsASnapshotAndSeqRestarts(t *testing.T) {
	for conn := 0; conn < 3; conn++ {
		h := startEvents(t, agentState("a", "stable", "h1"))
		st := h.stream()
		if snap := st.snapshot(t); snap.Seq != 0 || len(snap.Sessions) != 1 {
			t.Fatalf("connection %d snapshot = %+v", conn, snap)
		}
		h.obs.set(agentState("a", "busy", "h1"))
		h.clk.tick(t, time.Second)
		if ev := st.event(t); ev.Seq != 1 {
			t.Fatalf("connection %d first event seq = %d, want 1", conn, ev.Seq)
		}
	}
}

func TestHubClosingTheStreamReopensItWithAFreshSnapshot(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	first := h.stream()
	first.snapshot(t)
	h.obs.set(agentState("a", "busy", "h1"))
	h.clk.tick(t, time.Second)
	if ev := first.event(t); ev.Seq != 1 {
		t.Fatalf("seq = %d", ev.Seq)
	}

	first.endHub() // the Hub saw a gap and closed the stream
	waitFor2(t, first.wasReset)
	select {
	case <-h.opens:
		t.Fatal("the stream was reopened before the delay")
	case <-time.After(20 * time.Millisecond):
	}
	h.clk.tickDue(t, 5*time.Second)
	second := h.stream()
	snap := second.snapshot(t)
	if snap.Seq != 0 || snap.Sessions[0].State != "busy" {
		t.Fatalf("snapshot after reopen = %+v, want the current state", snap)
	}
	h.obs.set(agentState("a", "stable", "h1"))
	h.clk.tick(t, time.Second)
	if ev := second.event(t); ev.Seq != 1 {
		t.Fatalf("first event of the new stream has seq %d, want 1", ev.Seq)
	}
}

func TestWriteFailureReopensTheStream(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	first := h.stream()
	first.snapshot(t)
	first.failWrites(errors.New("broken pipe"))
	h.obs.set(agentState("a", "busy", "h1"))
	h.clk.tick(t, time.Second)
	waitFor2(t, first.wasReset)
	h.clk.tickDue(t, 5*time.Second)
	if snap := h.stream().snapshot(t); snap.Sessions[0].State != "busy" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestOpenFailureIsRetriedAfterTheDelay(t *testing.T) {
	hh := startEventsWithOpenError(t, errors.New("session busy"))
	hh.clk.tickDue(t, 5*time.Second)
	// The failed open sleeps for another delay: only then is the error cleared,
	// so the first retry cannot succeed by racing the test.
	waitFor2(t, func() bool { return hh.clk.dueIn(5 * time.Second) })
	hh.mu.Lock()
	hh.openErr = nil
	hh.mu.Unlock()
	hh.clk.tickDue(t, 5*time.Second)
	hh.stream().snapshot(t)
}

func startEventsWithOpenError(t *testing.T, err error) *evHarness {
	t.Helper()
	h := startEvents(t, agentState("a", "stable", "h1"))
	// startEvents already opened the first stream; consume and end it, then
	// make later opens fail.
	first := h.stream()
	first.snapshot(t)
	h.mu.Lock()
	h.openErr = err
	h.mu.Unlock()
	first.endHub()
	waitFor2(t, first.wasReset)
	return h
}

func TestObserveErrorKeepsTheStreamAndSkipsTheTick(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	st := h.stream()
	st.snapshot(t)
	h.obs.fail(errors.New("tmux: no server"))
	h.clk.tick(t, time.Second)
	waitFor2(t, func() bool { return h.obs.count() >= 2 })
	st.none(t) // a failed look is not "every session is gone"
	h.obs.fail(nil)
	h.obs.set(agentState("a", "busy", "h1"))
	h.clk.tick(t, time.Second)
	if ev := st.event(t); ev.Seq != 1 || ev.Event != EventStateChanged {
		t.Fatalf("event = %+v", ev)
	}
}

func TestNoSnapshotWhileTheFirstLookFails(t *testing.T) {
	obs := &scriptObserver{}
	obs.fail(errors.New("tmux: no server"))
	clk := newManualClock()
	pub, _ := NewEventPublisher(EventConfig{Observer: obs, Interval: time.Second, Clock: clk})
	opened := make(chan *fakeEvStream, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pub.serve(ctx, func(context.Context) (EventStream, error) {
			st := newFakeEvStream()
			opened <- st
			return st, nil
		})
		close(done)
	}()
	clk.tick(t, time.Second)
	select {
	case <-opened:
		t.Fatal("a stream was opened before the first successful look")
	case <-time.After(20 * time.Millisecond):
	}
	obs.fail(nil)
	obs.set(agentState("a", "stable", "h1"))
	clk.tick(t, time.Second)
	select {
	case st := <-opened:
		st.snapshot(t)
	case <-time.After(5 * time.Second):
		t.Fatal("no stream after the look succeeded")
	}
	cancel()
	<-done
}

func TestLinkEndClosesTheStreamGracefully(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	st := h.stream()
	st.snapshot(t)
	waitFor2(t, func() bool { return h.clk.waiters() > 0 })
	h.cancel()
	<-h.done
	if !st.wasClosed() || st.wasReset() {
		t.Fatalf("closed=%v reset=%v, want a graceful close", st.wasClosed(), st.wasReset())
	}
}

func TestFramesFromTheHubReachOnFrame(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	st := h.stream()
	st.snapshot(t)
	st.in <- json.RawMessage(`{"type":"turn.ack","turn_id":"x","status":"accepted"}`)
	select {
	case ty := <-h.acks:
		if ty != proto.TypeTurnAck {
			t.Fatalf("type = %q", ty)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn.ack not delivered")
	}
}

func TestEventTimeUsesTheClock(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	st := h.stream()
	snap := st.snapshot(t)
	if got := time.Time(snap.Time); !got.Equal(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("snapshot time = %v", got)
	}
	h.obs.set(agentState("a", "busy", "h1"))
	h.clk.tick(t, time.Second)
	if ev := st.event(t); !time.Time(ev.Time).Equal(time.Date(2026, 11, 9, 12, 0, 1, 0, time.UTC)) {
		t.Fatalf("event time = %v", time.Time(ev.Time))
	}
}

// Wire shape of the frames, pinned so that the Hub side can rely on it.
func TestFrameJSONShape(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"snapshot", SnapshotFrame{Type: proto.TypeEventsSnap, Time: ops.Timestamp(at), Sessions: []SessionState{{Name: "a", Kind: "shell", State: "unknown", ScreenHash: "ff"}}},
			`{"type":"events.snapshot","seq":0,"time":"2026-11-09T12:00:00.000Z","sessions":[{"name":"a","kind":"shell","state":"unknown","task":"","screen_hash":"ff"}]}`},
		{"snapshot with a task", SnapshotFrame{Type: proto.TypeEventsSnap, Time: ops.Timestamp(at), Sessions: []SessionState{{Name: "a", Kind: "shell", State: "stable", Task: "fix the build"}}},
			`{"type":"events.snapshot","seq":0,"time":"2026-11-09T12:00:00.000Z","sessions":[{"name":"a","kind":"shell","state":"stable","task":"fix the build"}]}`},
		{"empty snapshot", SnapshotFrame{Type: proto.TypeEventsSnap, Time: ops.Timestamp(at), Sessions: View{}.Sorted()},
			`{"type":"events.snapshot","seq":0,"time":"2026-11-09T12:00:00.000Z","sessions":[]}`},
		{"added", eventFrameOf(1, at, Change{EventSessionAdded, SessionState{Name: "a", Kind: "shell", State: "stable", ScreenHash: "ff"}}),
			`{"type":"event","seq":1,"time":"2026-11-09T12:00:00.000Z","event":"session_added","session":"a","kind":"shell","state":"stable","task":"","screen_hash":"ff"}`},
		{"added with a task", eventFrameOf(1, at, Change{EventSessionAdded, SessionState{Name: "a", Kind: "shell", State: "stable", Task: "t", ScreenHash: "ff"}}),
			`{"type":"event","seq":1,"time":"2026-11-09T12:00:00.000Z","event":"session_added","session":"a","kind":"shell","state":"stable","task":"t","screen_hash":"ff"}`},
		{"removed", eventFrameOf(2, at, Change{EventSessionRemoved, SessionState{Name: "a", Kind: "shell", State: "stable"}}),
			`{"type":"event","seq":2,"time":"2026-11-09T12:00:00.000Z","event":"session_removed","session":"a"}`},
		{"state", eventFrameOf(3, at, Change{EventStateChanged, SessionState{Name: "a", Kind: "shell", State: "busy", ScreenHash: "ff"}}),
			`{"type":"event","seq":3,"time":"2026-11-09T12:00:00.000Z","event":"state_changed","session":"a","kind":"shell","state":"busy","task":""}`},
		{"state with a task", eventFrameOf(3, at, Change{EventStateChanged, SessionState{Name: "a", Kind: "shell", State: "busy", Task: "x", ScreenHash: "ff"}}),
			`{"type":"event","seq":3,"time":"2026-11-09T12:00:00.000Z","event":"state_changed","session":"a","kind":"shell","state":"busy","task":"x"}`},
		{"screen", eventFrameOf(4, at, Change{EventScreenChanged, SessionState{Name: "a", Kind: "shell", State: "busy", ScreenHash: "ee"}}),
			`{"type":"event","seq":4,"time":"2026-11-09T12:00:00.000Z","event":"screen_changed","session":"a","screen_hash":"ee"}`},
		{"op.request", OpRequestFrame{Type: proto.TypeOpRequest, Op: "sessions_list", Actions: []ops.Action{ops.ActionRead}},
			`{"type":"op.request","op":"sessions_list","actions":["read"]}`},
		{"op.progress", OpProgressFrame{Type: proto.TypeOpProgress, Message: "x"}, `{"type":"op.progress","message":"x"}`},
		{"op.result", OpResultFrame{Type: proto.TypeOpResult, Result: json.RawMessage(`{"a":1}`)}, `{"type":"op.result","result":{"a":1}}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil || string(b) != c.want {
			t.Errorf("%s = %s (%v), want %s", c.name, b, err, c.want)
		}
	}
}

// ---- Diff, hash, observer, setting ----

func withTask(s SessionState, task string) SessionState { s.Task = task; return s }

func TestDiff(t *testing.T) {
	a := func(state, hash string) SessionState { return agentState("a", state, hash) }
	sh := SessionState{Name: "b", Kind: "shell", State: "unknown", ScreenHash: "s"}
	cases := []struct {
		name      string
		prev, cur View
		want      []string
	}{
		{"nothing", View{"a": a("stable", "1")}, View{"a": a("stable", "1")}, nil},
		{"both empty", nil, View{}, nil},
		{"added", View{}, View{"a": a("stable", "1")}, []string{"session_added:a"}},
		{"removed", View{"a": a("stable", "1")}, View{}, []string{"session_removed:a"}},
		{"state", View{"a": a("stable", "1")}, View{"a": a("busy", "1")}, []string{"state_changed:a"}},
		{"kind", View{"b": sh}, View{"b": {Name: "b", Kind: "other", State: "unknown", ScreenHash: "s"}}, []string{"state_changed:b"}},
		{"screen", View{"a": a("stable", "1")}, View{"a": a("stable", "2")}, []string{"screen_changed:a"}},
		{"task only", View{"a": a("stable", "1")}, View{"a": withTask(a("stable", "1"), "new task")}, []string{"state_changed:a"}},
		{"task cleared", View{"a": withTask(a("stable", "1"), "old")}, View{"a": a("stable", "1")}, []string{"state_changed:a"}},
		{"state and screen", View{"a": a("stable", "1")}, View{"a": a("busy", "2")}, []string{"state_changed:a", "screen_changed:a"}},
		{"ordered by name", View{"z": a("stable", "1")}, View{"b": sh, "a": a("stable", "1")}, []string{"session_added:a", "session_added:b", "session_removed:z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, ch := range Diff(c.prev, c.cur) {
				got = append(got, ch.Event+":"+ch.Session.Name)
			}
			if !equalStrings(got, c.want) {
				t.Fatalf("Diff = %v, want %v", got, c.want)
			}
		})
	}
}

func TestScreenHash(t *testing.T) {
	base := ops.Screen{Width: 80, Height: 24, Lines: []string{"hello", "world"}, Spans: []ops.StyleSpan{{Line: 0, Start: 0, End: 2, Bold: true}}}
	h := ScreenHash(base)
	if len(h) != 32 || h != ScreenHash(base) {
		t.Fatalf("hash %q is not stable hex", h)
	}
	moved := base
	moved.Cursor = &ops.Cursor{Row: 1, Col: 3}
	if ScreenHash(moved) != h {
		t.Fatal("the cursor changed the hash")
	}
	for name, mut := range map[string]func(*ops.Screen){
		"text":   func(s *ops.Screen) { s.Lines = []string{"hello", "worle"} },
		"size":   func(s *ops.Screen) { s.Width = 81 },
		"spans":  func(s *ops.Screen) { s.Spans = nil },
		"lines+": func(s *ops.Screen) { s.Lines = append([]string{"x"}, s.Lines...) },
	} {
		s := base
		mut(&s)
		if ScreenHash(s) == h {
			t.Errorf("a change of %s kept the hash", name)
		}
	}
	if strings.Contains(h, "hello") {
		t.Fatal("hash leaks content")
	}
}

// AC 3: an unshared session appears in no event.
func TestHostObserverShowsOnlySharedSessions(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"open", "secret"} {
		ref := e.host.addSession(n, "claude")
		e.host.setScreen(ref, "content of "+n)
	}
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	obs, err := NewHostObserver(HostObserverConfig{Host: e.host, Classifier: e.cls, Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	v, err := obs.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || v["open"].Kind != "agent:claude" || v["open"].State != "stable" || len(v["open"].ScreenHash) != 32 {
		t.Fatalf("view = %+v", v)
	}
	if _, ok := v["secret"]; ok {
		t.Fatal("an unshared session is in the view")
	}

	// Through the publisher: no frame names the unshared session.
	clk := newManualClock()
	pub, _ := NewEventPublisher(EventConfig{Observer: obs, Interval: time.Second, Clock: clk})
	st := newFakeEvStream()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pub.serve(ctx, func(context.Context) (EventStream, error) { return st, nil })
		close(done)
	}()
	defer func() { cancel(); <-done }()
	if snap := st.snapshot(t); len(snap.Sessions) != 1 || snap.Sessions[0].Name != "open" {
		t.Fatalf("snapshot = %+v", snap)
	}
	// Activity in the unshared session produces nothing.
	e.host.mu.Lock()
	e.host.screens["%2"] = ops.Screen{Lines: []string{"changed secret"}}
	e.host.mu.Unlock()
	clk.tick(t, time.Second)
	st.none(t)

	// Sharing it announces it, withdrawing the share removes it again, and
	// neither frame carries content.
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	clk.tick(t, time.Second)
	if ev := st.event(t); ev.Event != EventSessionAdded || ev.Session != "secret" || ev.Seq != 1 {
		t.Fatalf("event = %+v", ev)
	}
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	clk.tick(t, time.Second)
	if ev := st.event(t); ev.Event != EventSessionRemoved || ev.Session != "secret" || ev.Seq != 2 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestHostObserverKeepsUnreadablePanesUnknown(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("a", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.mu.Lock()
	e.host.sessions[0].Panes = nil // a session without a pane
	e.host.mu.Unlock()
	obs, _ := NewHostObserver(HostObserverConfig{Host: e.host, Classifier: e.cls, Policy: e.policy})
	v, err := obs.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := v["a"]; s.Kind != ops.PaneKindUnknown || s.State != ops.PaneStateUnknown || s.ScreenHash != "" {
		t.Fatalf("state = %+v", s)
	}

	e.host.mu.Lock()
	e.host.listErr = errors.New("no server")
	e.host.mu.Unlock()
	if _, err := obs.Observe(context.Background()); err == nil {
		t.Fatal("a failing listing was reported as an empty view")
	}
	if _, err := NewHostObserver(HostObserverConfig{}); err == nil {
		t.Fatal("an observer without ports was accepted")
	}
}

func TestPollIntervalSetting(t *testing.T) {
	for in, want := range map[string]time.Duration{"1s": time.Second, "250ms": 250 * time.Millisecond, "1m": time.Minute, "100ms": 100 * time.Millisecond} {
		if got, err := ParsePollInterval(in); err != nil || got != want {
			t.Errorf("ParsePollInterval(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "fast", "50ms", "2m", "-1s", "0"} {
		if _, err := ParsePollInterval(in); err == nil {
			t.Errorf("ParsePollInterval(%q) was accepted", in)
		}
	}
	if d, err := ResolvePollInterval(nil); err != nil || d != DefaultPollInterval {
		t.Fatalf("default = %v, %v", d, err)
	}
	res := func(env map[string]string) *config.Resolver {
		return &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	}
	if d, err := ResolvePollInterval(res(nil)); err != nil || d != DefaultPollInterval {
		t.Fatalf("resolved default = %v, %v", d, err)
	}
	if d, err := ResolvePollInterval(res(map[string]string{"MINION_SESSION_POLL_INTERVAL": "300ms"})); err != nil || d != 300*time.Millisecond {
		t.Fatalf("from environment = %v, %v", d, err)
	}
	if _, err := ResolvePollInterval(res(map[string]string{"MINION_SESSION_POLL_INTERVAL": "1ms"})); err == nil {
		t.Fatal("a too small interval was accepted")
	}
}

// A set_task-style change produces state_changed with the new task; a cleared
// task is "" on the wire; an unshared session's task never appears.
func TestHostObserverReportsTaskChangesAndHidesUnsharedTasks(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"open", "secret"} {
		ref := e.host.addSession(n, "claude")
		e.host.setScreen(ref, "content of "+n)
	}
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.host.SetOption(ctx, "open", "@minion_task", "first task"); err != nil {
		t.Fatal(err)
	}
	if err := e.host.SetOption(ctx, "secret", "@minion_task", "TOP-SECRET-TASK"); err != nil {
		t.Fatal(err)
	}
	obs, err := NewHostObserver(HostObserverConfig{Host: e.host, Classifier: e.cls, Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	clk := newManualClock()
	pub, _ := NewEventPublisher(EventConfig{Observer: obs, Interval: time.Second, Clock: clk})
	st := newFakeEvStream()
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		pub.serve(pctx, func(context.Context) (EventStream, error) { return st, nil })
		close(done)
	}()
	defer func() { cancel(); <-done }()

	raw := st.next(t)
	if strings.Contains(string(raw), "TOP-SECRET-TASK") {
		t.Fatalf("the snapshot carries an unshared task: %s", raw)
	}
	var snap SnapshotFrame
	if err := json.Unmarshal(raw, &snap); err != nil || len(snap.Sessions) != 1 || snap.Sessions[0].Task != "first task" {
		t.Fatalf("snapshot = %s (%v)", raw, err)
	}

	next := func(want string) EventFrame {
		clk.tick(t, time.Second)
		raw := st.next(t)
		if strings.Contains(string(raw), "TOP-SECRET") {
			t.Fatalf("a frame carries an unshared task: %s", raw)
		}
		var ev EventFrame
		if err := json.Unmarshal(raw, &ev); err != nil || ev.Event != EventStateChanged || ev.Session != "open" {
			t.Fatalf("frame = %s (%v), want state_changed for open", raw, err)
		}
		if ev.Task == nil || *ev.Task != want || !strings.Contains(string(raw), `"task":`+mustJSONString(t, want)) {
			t.Fatalf("frame = %s, want task %q", raw, want)
		}
		return ev
	}
	if err := e.host.SetOption(ctx, "open", "@minion_task", "second task"); err != nil {
		t.Fatal(err)
	}
	next("second task")
	if err := e.host.SetOption(ctx, "secret", "@minion_task", "TOP-SECRET-CHANGED"); err != nil {
		t.Fatal(err)
	}
	clk.tick(t, time.Second)
	st.none(t) // the unshared session's task change is in no frame
	if err := e.host.SetOption(ctx, "open", "@minion_task", ""); err != nil {
		t.Fatal(err)
	}
	next("")
}

func mustJSONString(t testing.TB, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// AC 3: a share withdrawn while the panes are read keeps the session's new
// state and screen hash out of the view.
func TestHostObserverDropsSessionsRevokedDuringTheObservation(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "private now")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.mu.Lock()
	e.host.onSnapshot = func() {
		if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareNone}); err != nil {
			t.Error(err)
		}
	}
	e.host.mu.Unlock()
	obs, _ := NewHostObserver(HostObserverConfig{Host: e.host, Classifier: e.cls, Policy: e.policy})
	v, err := obs.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("a session revoked during the observation is in the view: %+v", v)
	}
}

// Fix 2: a module registers for inbound frames after the publisher serves.
func TestOnFrameRegistrationAfterServeStarted(t *testing.T) {
	h := startEvents(t, agentState("a", "stable", "h1"))
	st := h.stream()
	st.snapshot(t)
	got := make(chan json.RawMessage, 2)
	h.pub.OnFrame(proto.TypeTurnAck, func(body json.RawMessage) { got <- body })
	other := make(chan struct{}, 1)
	h.pub.OnFrame("something.else", func(json.RawMessage) { other <- struct{}{} })
	st.in <- json.RawMessage(`{"type":"turn.ack","turn_id":"x","status":"accepted"}`)
	select {
	case body := <-got:
		if !strings.Contains(string(body), `"turn_id":"x"`) {
			t.Fatalf("body = %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registered handler did not get the turn.ack")
	}
	select {
	case <-other:
		t.Fatal("a handler of another frame type was called")
	default:
	}
	// The configured OnFrame still sees it, and a registration survives a new stream.
	if ty := <-h.acks; ty != proto.TypeTurnAck {
		t.Fatalf("configured OnFrame got %q", ty)
	}
	st.endHub()
	waitFor2(t, st.wasReset)
	h.clk.tickDue(t, 5*time.Second)
	st2 := h.stream()
	st2.snapshot(t)
	st2.in <- json.RawMessage(`{"type":"turn.ack","turn_id":"y","status":"accepted"}`)
	select {
	case body := <-got:
		if !strings.Contains(string(body), `"turn_id":"y"`) {
			t.Fatalf("body = %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registration did not survive the new stream")
	}
}
