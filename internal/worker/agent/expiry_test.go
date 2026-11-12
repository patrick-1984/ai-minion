package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// fakeWM is a WallMono whose two clocks the test moves independently.
type fakeWM struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newFakeWM() *fakeWM {
	return &fakeWM{wall: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC), mono: 10 * time.Hour}
}

func (c *fakeWM) Wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *fakeWM) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *fakeWM) set(wall time.Time, mono time.Duration) {
	c.mu.Lock()
	c.wall, c.mono = wall, mono
	c.mu.Unlock()
}

// expiryAt returns the Expiry of a certificate that ends d after the clock's now:
// the wall end and the monotonic deadline of the window check.
func (c *fakeWM) expiryAfter(d time.Duration) transport.Expiry {
	return transport.Expiry{WallMs: c.Wall().Add(d).UnixMilli(), DeadlineMono: c.Mono() + d}
}

func TestExpiredIsOverWhenEitherClockReachesTheEnd(t *testing.T) {
	c := newFakeWM()
	e := c.expiryAfter(time.Hour)
	start, mono := c.Wall(), c.Mono()
	cases := []struct {
		name string
		wall time.Time
		mono time.Duration
		want bool
	}{
		{"both before", start.Add(time.Hour - time.Millisecond), mono + time.Hour - time.Millisecond, false},
		{"wall exactly at not_after, mono not reached", start.Add(time.Hour), mono + time.Minute, true},
		{"mono exactly at the deadline, wall not reached", start.Add(time.Minute), mono + time.Hour, true},
		{"wall one millisecond past", start.Add(time.Hour + time.Millisecond), mono, true},
		{"wall moved backwards, mono not reached: no extension, not over", start.Add(-24 * time.Hour), mono + time.Minute, false},
		{"wall moved backwards, mono at the deadline: over", start.Add(-24 * time.Hour), mono + time.Hour, true},
		{"after a sleep the wall clock is far ahead while mono barely moved", start.Add(48 * time.Hour), mono + time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.set(tc.wall, tc.mono)
			if got := e.Expired(c); got != tc.want {
				t.Fatalf("Expired = %v, want %v", got, tc.want)
			}
		})
	}
}

// expiringFixture connects a connector to a live session pair whose attempt
// carries the given expiry, and captures the link.
func expiringFixture(t *testing.T, wm *fakeWM, exp transport.Expiry) (*connFixture, *liveHub, chan *Link) {
	t.Helper()
	f := newConnFixture(t, func(cfg *ConnectorConfig) { cfg.WallMono = wm })
	live := newLiveHub(t)
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		e := exp
		return &Attempt{
			Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: live.hash, Version: proto.Current, Expiry: &e,
			Reply: Reply{Type: proto.TypeWelcome, Welcome: okWelcome()}, Session: live.workerSess,
		}, nil
	})
	links := make(chan *Link, 4)
	f.conn.OnLink("capture", func(l *Link) { links <- l; <-l.Context().Done() })
	f.conn.OnLink("revoke", f.conn.ServeRevokes)
	f.run()
	return f, live, links
}

// tryFrame writes a frame on a new op stream and reports whether an answer
// came back; a stream that the Worker closed or reset gives false without a
// test failure.
func tryFrame(t *testing.T, live *liveHub, frame any) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := live.hubSess.OpenOp(ctx)
	if err != nil {
		return false
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(5 * time.Second))
	if st.WriteFrame(frame) != nil {
		return false
	}
	_, err = st.ReadFrame()
	return err == nil
}

func TestWallClockJumpToExactlyNotAfterClosesTheSessionWhileMonotonicIsBefore(t *testing.T) {
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	f, live, _ := expiringFixture(t, wm, exp)
	f.waitState(ConnConnected)
	// The wall clock jumps to exactly not_after; the monotonic clock is far from
	// its deadline. The next stream (either direction) finds the end.
	wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono()+time.Second)
	if tryFrame(t, live, revokeFor(t, f.hub, live.hash)) {
		t.Fatal("a stream at not_after was answered")
	}
	eventually(t, func() bool { return f.conn.Status().State != ConnConnected })
	// The enrollment and the pin are untouched.
	if enr := f.storedEnrollment(); enr.Pin.Pub == nil {
		t.Fatal("expiry removed the pin")
	}
	if !strings.Contains(f.logs.String(), "reached its not_after") {
		t.Errorf("logs = %s", f.logs.String())
	}
}

func TestDeadlineTimerClosesAHubThatNeverDisconnects(t *testing.T) {
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	f, _, _ := expiringFixture(t, wm, exp)
	f.waitState(ConnConnected)
	// Nothing happens on the connection; only the monotonic timer can end it.
	wm.set(wm.Wall(), exp.DeadlineMono)
	eventually(t, func() bool { return f.clk.pending() > 0 })
	f.clk.Advance(time.Hour)
	eventually(t, func() bool { return f.conn.Status().State != ConnConnected })
	f.storedEnrollment()
}

func TestAcceptedAtExactlyNotAfterAdmitsNothing(t *testing.T) {
	wm := newFakeWM()
	exp := wm.expiryAfter(0) // now == not_after: accepted by the window, no time left
	_, live, _ := expiringFixture(t, wm, exp)
	// The connection is closed at once: the session ends without an operation.
	select {
	case <-live.workerSess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a connection accepted at exactly not_after stayed open")
	}
}

func TestRevokeAtOrAfterTheDeadlineIsNotAppliedAndThePairingStays(t *testing.T) {
	for _, tc := range []struct {
		name string
		wall func(transport.Expiry) time.Time
	}{
		{"exactly at not_after", func(e transport.Expiry) time.Time { return time.UnixMilli(e.WallMs).UTC() }},
		{"after not_after", func(e transport.Expiry) time.Time { return time.UnixMilli(e.WallMs + 5).UTC() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wm := newFakeWM()
			exp := wm.expiryAfter(time.Hour)
			f, live, links := expiringFixture(t, wm, exp)
			f.waitState(ConnConnected)
			link := <-links
			wm.set(tc.wall(exp), wm.Mono())
			raw, _ := json.Marshal(revokeFor(t, f.hub, live.hash))
			if err := f.conn.HandleRevoke(link, raw, func() { t.Error("a revoke after the deadline was confirmed") }); !errors.Is(err, ErrRevokeRejected) {
				t.Fatalf("HandleRevoke = %v, want ErrRevokeRejected", err)
			}
			if _, ok, err := f.enr.Load(context.Background()); err != nil || !ok {
				t.Fatalf("the pairing must stay: ok %v err %v", ok, err)
			}
			eventually(t, func() bool { return f.conn.Status().State != ConnConnected })
		})
	}
}

// flipStream is a fake op stream that moves the clocks when the request has
// been read: the request is decoded before the deadline and admitted after it.
type flipStream struct {
	*fakeOpStream
	onRead func()
}

func (s *flipStream) ReadFrame() (json.RawMessage, error) {
	b, err := s.fakeOpStream.ReadFrame()
	if err == nil && s.onRead != nil {
		f := s.onRead
		s.onRead = nil
		f()
	}
	return b, err
}

func TestAnOperationDecodedBeforeTheDeadlineAndAdmittedAfterItIsNotAdmitted(t *testing.T) {
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	var closed bool
	g := &expiryGuard{exp: exp, clk: wm, log: nopLogger(), onExpire: func() { closed = true }}
	ctx := withGuard(context.Background(), g)
	ex := &fakeExec{}
	srv := newOpServer(t, ex)
	st := &flipStream{
		fakeOpStream: newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil)),
		onRead:       func() { wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono()) },
	}
	srv.ServeStream(ctx, st)
	if n := len(ex.requests()); n != 0 {
		t.Fatalf("the executor ran %d requests after the deadline", n)
	}
	if !closed || !st.wasReset() {
		t.Fatalf("closed %v, stream reset %v: the session must close and the Hub sees a lost stream", closed, st.wasReset())
	}
}

func TestAnOperationBeforeTheDeadlineIsAdmitted(t *testing.T) {
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	g := &expiryGuard{exp: exp, clk: wm, log: nopLogger()}
	ex := &fakeExec{}
	srv := newOpServer(t, ex)
	st := newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	srv.ServeStream(withGuard(context.Background(), g), st)
	if n := len(ex.requests()); n != 1 {
		t.Fatalf("requests = %d", n)
	}
}

func TestExpiryDuringAWriteLetsTheWriteFinishAndFailsTheRead(t *testing.T) {
	for _, tc := range []struct {
		op        string
		wantReset bool
	}{
		{ops.OpSendText, false},    // a write that started completes and is delivered
		{ops.OpSessionsList, true}, // a read in flight fails: the Hub sees connection_lost
	} {
		t.Run(tc.op, func(t *testing.T) {
			wm := newFakeWM()
			exp := wm.expiryAfter(time.Hour)
			linkCtx, cancel := context.WithCancelCause(context.Background())
			g := &expiryGuard{exp: exp, clk: wm, log: nopLogger(), onExpire: func() { cancel(ErrLinkLost) }}
			ctx := withGuard(linkCtx, g)
			ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
				// The deadline passes while the operation runs.
				wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono())
				g.check()
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return json.RawMessage(`{"ok":true}`), nil
			}}
			srv := newOpServer(t, ex)
			var req any
			if tc.op == ops.OpSendText {
				req = opRequest(tc.op, allActions, sendText("work", "hi", false))
			} else {
				req = opRequest(tc.op, []ops.Action{ops.ActionRead}, nil)
			}
			st := newFakeOpStream(t, req)
			srv.ServeStream(ctx, st)
			if got := st.wasReset(); got != tc.wantReset {
				t.Fatalf("reset = %v, want %v (frames %v)", got, tc.wantReset, st.types(t))
			}
		})
	}
}

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A revoke whose deadline passes after the first check (during the
// acknowledgement, or while the wipe waits for the enrollment lock) is not
// applied: the check is repeated inside the guard, right before the wipe.
func TestRevokeIsRecheckedInsideTheGuardBeforeTheWipe(t *testing.T) {
	for _, tc := range []string{"during the acknowledgement", "during lock contention"} {
		t.Run(tc, func(t *testing.T) {
			wm := newFakeWM()
			exp := wm.expiryAfter(time.Hour)
			f, live, links := expiringFixture(t, wm, exp)
			f.waitState(ConnConnected)
			link := <-links
			past := func() { wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono()) }
			raw, _ := json.Marshal(revokeFor(t, f.hub, live.hash))

			var err error
			if tc == "during the acknowledgement" {
				err = f.conn.HandleRevoke(link, raw, past)
			} else {
				reached := make(chan struct{})
				f.enr.mu.Lock() // the wipe waits for the enrollment guard
				done := make(chan error, 1)
				go func() { done <- f.conn.HandleRevoke(link, raw, func() { close(reached) }) }()
				<-reached
				past()
				f.enr.mu.Unlock()
				err = <-done
			}
			if !errors.Is(err, ErrRevokeRejected) {
				t.Fatalf("HandleRevoke = %v, want ErrRevokeRejected", err)
			}
			if _, ok, lerr := f.enr.Load(context.Background()); lerr != nil || !ok {
				t.Fatalf("the pairing must stay: ok %v err %v", ok, lerr)
			}
		})
	}
}
