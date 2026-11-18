package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// A write admitted before the pause completes and answers; new streams are
// refused while paused, and admitted again after Reopen.
func TestPauseAdmittedWriteCompletes(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		if rq.Op == ops.OpSendText {
			started <- struct{}{}
			<-release
			return json.RawMessage(`{"outcome":"delivered"}`), nil
		}
		return json.RawMessage(`{"sessions":[]}`), nil
	}}
	srv := newOpServer(t, ex)
	link, cancel, hub := hubLink(t)
	defer cancel(nil)
	go srv.Serve(link)
	write := hubRequest(t, hub, opRequest(ops.OpSendText, allActions, sendText("work", "hi", false)))
	<-started

	done := make(chan int, 1)
	go func() {
		n, _ := srv.Suspend(context.Background())
		done <- n
	}()
	// Admission is closed at once, while the write still runs.
	eventuallyTrue(t, func() bool { return !srv.Admitting() })
	late := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	if ty, body := readTyped(t, late); ty != proto.TypeError || !strings.Contains(string(body), "paused") {
		t.Fatalf("a stream while paused got %q %s", ty, body)
	}
	select {
	case n := <-done:
		t.Fatalf("Suspend returned (%d pending) before the write finished", n)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if n := <-done; n != 0 {
		t.Fatalf("pending = %d after the write finished", n)
	}
	if ty, body := readTyped(t, write); ty != proto.TypeOpResult || !strings.Contains(string(body), "delivered") {
		t.Fatalf("the admitted write answered %q %s", ty, body)
	}
	srv.Reopen()
	again := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	if ty, body := readTyped(t, again); ty != proto.TypeOpResult {
		t.Fatalf("after Reopen a read got %q %s", ty, body)
	}
}

// The drain ends at its deadline: the ack reports the write still running
// and admission stays closed.
func TestPauseDrainTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 1)
	ex := &fakeExec{run: func(context.Context, Request) (json.RawMessage, error) {
		started <- struct{}{}
		<-release
		return json.RawMessage(`{"outcome":"delivered"}`), nil
	}}
	srv := newOpServer(t, ex)
	link, cancel, hub := hubLink(t)
	defer cancel(nil)
	go srv.Serve(link)
	hubRequest(t, hub, opRequest(ops.OpSendText, allActions, sendText("work", "hi", false)))
	<-started

	svc := &PauseService{dir: t.TempDir(), log: discardLogger(), now: time.Now, set: SetPause, drain: 50 * time.Millisecond,
		afterFunc: func(time.Duration, func()) func() bool { return func() bool { return true } }, gate: NewPauseGate(false)}
	svc.attach(srv)
	res, err := svc.Pause(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.WritesPending != 1 || !res.Paused {
		t.Fatalf("pause ack = %+v, want writes_pending 1", res)
	}
	if srv.Admitting() {
		t.Fatal("admission reopened after the drain timeout")
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"writes_pending":1`) {
		t.Fatalf("ack JSON = %s", b)
	}
}

// pauseTimers is an injectable clock for timed pauses.
type pauseTimers struct {
	mu  sync.Mutex
	now time.Time
	fns []func()
}

func (f *pauseTimers) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *pauseTimers) AfterFunc(_ time.Duration, fn func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fns = append(f.fns, fn)
	return func() bool { return true }
}

// fire runs every armed timer, the way they would fire late.
func (f *pauseTimers) fire() {
	f.mu.Lock()
	fns := f.fns
	f.fns = nil
	f.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// The timer of a timed pause never ends the indefinite pause that replaced
// it; a timer that still matches ends its pause.
func TestTimedPauseReplacedByIndefinite(t *testing.T) {
	clk := &pauseTimers{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	svc, err := newPauseService(dir, discardLogger(), clk.Now, clk.AfterFunc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pause(context.Background(), clk.now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pause(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	clk.fire() // the 30-minute timer fires late
	if !svc.Paused() {
		t.Fatal("the old timer ended the indefinite pause")
	}
	if p, _ := ReadPause(dir); !p.Paused || !p.Until.IsZero() {
		t.Fatalf("saved = %+v", p)
	}
	// A timed pause whose timer still matches ends.
	if _, err := svc.Pause(context.Background(), clk.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	clk.fire()
	if svc.Paused() {
		t.Fatal("the matching timer did not end the pause")
	}
	if p, _ := ReadPause(dir); p.Paused {
		t.Fatalf("saved = %+v, want resumed", p)
	}
}

// A timed pause that ended before the start counts as resumed; one that runs
// on starts paused with its timer armed.
func TestExpiredPauseAtStart(t *testing.T) {
	clk := &pauseTimers{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	if _, err := SetPause(dir, true, clk.now.Add(-time.Minute), clk.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	svc, err := newPauseService(dir, discardLogger(), clk.Now, clk.AfterFunc)
	if err != nil {
		t.Fatal(err)
	}
	if svc.Paused() || len(clk.fns) != 0 {
		t.Fatalf("an expired pause started paused (timers %d)", len(clk.fns))
	}
	if _, err := SetPause(dir, true, clk.now.Add(time.Hour), clk.now); err != nil {
		t.Fatal(err)
	}
	svc, err = newPauseService(dir, discardLogger(), clk.Now, clk.AfterFunc)
	if err != nil {
		t.Fatal(err)
	}
	if !svc.Paused() || len(clk.fns) != 1 {
		t.Fatalf("a running timed pause: paused %t, timers %d", svc.Paused(), len(clk.fns))
	}
	clk.fire()
	if svc.Paused() {
		t.Fatal("the timer armed at the start did not end the pause")
	}
}

// A pause whose file cannot be written is not applied and not acknowledged.
func TestPauseSaveFailureChangesNothing(t *testing.T) {
	svc, err := newPauseService(t.TempDir(), discardLogger(), time.Now, func(time.Duration, func()) func() bool { return func() bool { return true } })
	if err != nil {
		t.Fatal(err)
	}
	svc.set = func(string, bool, time.Time, time.Time) (PauseState, error) {
		return PauseState{}, errors.New("disk full")
	}
	if _, err := svc.Pause(context.Background(), time.Time{}); err == nil {
		t.Fatal("a failed save was acknowledged")
	}
	if svc.Paused() {
		t.Fatal("the gate was set although nothing was saved")
	}
}

// The connector does not dial while paused, whether the pause came during
// the backoff or while connected, and dials again after the resume.
func TestPauseDuringHandshakeBackoffConnected(t *testing.T) {
	gate := NewPauseGate(false)
	f := newConnFixture(t, func(c *ConnectorConfig) { c.Pause = gate })
	failing := true
	var mu sync.Mutex
	dials := 0
	sess := newFakeSession()
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if failing {
			return nil, errors.New("connection refused")
		}
		return &Attempt{Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: randomHash(), Version: proto.Current,
			Reply: Reply{Type: proto.TypeWelcome, Welcome: okWelcome()}, Session: sess}, nil
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return dials }
	quiet := func(what string) {
		t.Helper()
		eventuallyTrue(t, func() bool { st := f.conn.Status(); return st.State == ConnIdle && st.Reason == ReasonPaused })
		n := count()
		for i := 0; i < 5; i++ {
			f.clk.Advance(10 * time.Minute) // every backoff and idle wait ends
			time.Sleep(10 * time.Millisecond)
		}
		if count() != n {
			t.Fatalf("%s: dialed %d times while paused", what, count()-n)
		}
	}
	f.run()
	// In the backoff: the pause wakes the wait and nothing is dialed.
	eventuallyTrue(t, func() bool { return count() >= 1 && f.conn.Status().State == ConnBackoff })
	gate.Set(true)
	quiet("backoff")
	// Resumed: it connects.
	mu.Lock()
	failing = false
	mu.Unlock()
	gate.Set(false)
	eventuallyTrue(t, func() bool {
		f.clk.Advance(time.Minute)
		return f.conn.Status().State == ConnConnected
	})
	// Connected: the pause ends the link and no dial follows.
	gate.Set(true)
	quiet("connected")
	// Paused before the start: never dials.
	gate2 := NewPauseGate(true)
	g := newConnFixture(t, func(c *ConnectorConfig) { c.Pause = gate2 })
	g.dial.script(func(ConnectParams) (*Attempt, error) {
		t.Error("a paused connector dialed")
		return nil, errors.New("x")
	})
	g.run()
	eventuallyTrue(t, func() bool { st := g.conn.Status(); return st.State == ConnIdle && st.Reason == ReasonPaused })
	for i := 0; i < 5; i++ {
		g.clk.Advance(10 * time.Minute)
		time.Sleep(10 * time.Millisecond)
	}
}

func eventuallyTrue(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
