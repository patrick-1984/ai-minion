package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type turnHarness struct {
	t      *testing.T
	em     *TurnEmitter
	out    TurnOutbox
	clk    *manualClock
	cancel context.CancelFunc
	done   chan struct{}
	opens  chan *fakeEvStream
	mu     sync.Mutex
	openFn func() error
}

// fakeShares is a ShareChecker whose answer a test sets.
type fakeShares struct {
	mu       sync.Mutex
	unshared map[string]bool
	err      error
}

func (f *fakeShares) Shared(_ context.Context, session string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return !f.unshared[session], nil
}

func (f *fakeShares) set(session string, shared bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unshared == nil {
		f.unshared = map[string]bool{}
	}
	f.unshared[session] = !shared
}

func (f *fakeShares) failWith(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

// newEmitter builds an emitter that shares every session unless cfg says
// otherwise.
func newEmitter(t testing.TB, cfg TurnConfig) *TurnEmitter {
	t.Helper()
	if cfg.Shares == nil {
		cfg.Shares = &fakeShares{}
	}
	em, err := NewTurnEmitter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return em
}

func startTurns(t *testing.T) *turnHarness { return startTurnsShares(t, &fakeShares{}) }

func startTurnsShares(t *testing.T, sh ShareChecker) *turnHarness {
	t.Helper()
	h := &turnHarness{t: t, clk: newManualClock(), opens: make(chan *fakeEvStream, 16), done: make(chan struct{}), out: NewMemOutbox(0)}
	h.em = newEmitter(t, TurnConfig{Shares: sh, Outbox: h.out, Clock: h.clk, AckTimeout: 10 * time.Second, ReopenDelay: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { cancel(); <-h.done })
	open := func(context.Context) (EventStream, error) {
		h.mu.Lock()
		fn := h.openFn
		h.mu.Unlock()
		if fn != nil {
			if err := fn(); err != nil {
				return nil, err
			}
		}
		st := newFakeEvStream()
		h.opens <- st
		return st, nil
	}
	go func() { h.em.serve(ctx, open); close(h.done) }()
	return h
}

func (h *turnHarness) stream() *fakeEvStream {
	h.t.Helper()
	select {
	case st := <-h.opens:
		return st
	case <-time.After(30 * time.Second):
		h.t.Fatal("no turn channel was opened")
		return nil
	}
}

// open reads the turns.open frame.
func (s *fakeEvStream) open(t testing.TB) {
	t.Helper()
	f := s.next(t)
	if ty, _ := proto.FrameType(f); ty != FrameTurnsOpen {
		t.Fatalf("first frame is %s, want %s", f, FrameTurnsOpen)
	}
}

func (s *fakeEvStream) turn(t testing.TB) turnFrame {
	t.Helper()
	f := s.next(t)
	var tf turnFrame
	if err := json.Unmarshal(f, &tf); err != nil || tf.Type != FrameTurn {
		t.Fatalf("frame %s is not a turn (%v)", f, err)
	}
	return tf
}

func (s *fakeEvStream) ack(id, status string) {
	b, _ := json.Marshal(map[string]string{"type": proto.TypeTurnAck, "turn_id": id, "status": status})
	s.in <- b
}

func pendingIDs(t *testing.T, o TurnOutbox) []string {
	t.Helper()
	p, err := o.Pending()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range p {
		ids = append(ids, x.TurnID)
	}
	return ids
}

func waitPending(t *testing.T, o TurnOutbox, want int) {
	t.Helper()
	waitFor2(t, func() bool { p, _ := o.Pending(); return len(p) == want })
}

func TestTurnEmittedBeforeAnyConnectionIsSentOnConnect(t *testing.T) {
	h := &turnHarness{t: t, clk: newManualClock(), opens: make(chan *fakeEvStream, 4), done: make(chan struct{}), out: NewMemOutbox(0)}
	h.em = newEmitter(t, TurnConfig{Outbox: h.out, Clock: h.clk})
	id, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "while offline"})
	if err != nil || !ops.ValidUUIDv7(id) {
		t.Fatalf("Emit = %q, %v", id, err)
	}
	if got := pendingIDs(t, h.out); len(got) != 1 || got[0] != id {
		t.Fatalf("pending = %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); <-h.done })
	h.cancel = cancel
	go func() {
		h.em.serve(ctx, func(context.Context) (EventStream, error) { st := newFakeEvStream(); h.opens <- st; return st, nil })
		close(h.done)
	}()
	st := h.stream()
	st.open(t)
	tf := st.turn(t)
	if tf.TurnID != id || tf.Session != "minion-1" || tf.Text != "while offline" || time.Time(tf.Time).IsZero() {
		t.Fatalf("turn = %+v", tf)
	}
	st.ack(id, "stored")
	waitPending(t, h.out, 0)
}

func TestAckedTurnIsNeverSentAgain(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "one"})
	if tf := st.turn(t); tf.TurnID != id {
		t.Fatalf("turn = %+v", tf)
	}
	st.ack(id, "stored")
	waitPending(t, h.out, 0)
	h.clk.tick(t, 11*time.Second)
	st.none(t)
}

func TestUnackedTurnIsResentAfterTheAckTimeoutWithTheSameID(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "one"})
	first := st.turn(t)
	h.clk.tick(t, 5*time.Second)
	st.none(t) // too early
	h.clk.tick(t, 6*time.Second)
	again := st.turn(t)
	if again.TurnID != id || first.TurnID != id || again.Text != "one" {
		t.Fatalf("resent turn = %+v", again)
	}
	st.ack(id, "stored")
	waitPending(t, h.out, 0)
}

func TestUnackedTurnsSurviveAReconnectInOrderAndWithoutDuplicates(t *testing.T) {
	h := startTurns(t)
	st1 := h.stream()
	st1.open(t)
	a, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "a"})
	b, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "b"})
	c, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-2", Text: "c"})
	got := []string{st1.turn(t).TurnID, st1.turn(t).TurnID, st1.turn(t).TurnID}
	if got[0] != a || got[1] != b || got[2] != c {
		t.Fatalf("send order = %v, want %v", got, []string{a, b, c})
	}
	st1.ack(b, "stored") // only the second one was acknowledged
	waitPending(t, h.out, 2)

	st1.endHub() // the connection drops
	waitFor2(t, st1.wasReset)
	h.clk.tick(t, 2*time.Second)

	st2 := h.stream()
	st2.open(t)
	again := []string{st2.turn(t).TurnID, st2.turn(t).TurnID}
	if again[0] != a || again[1] != c {
		t.Fatalf("after the reconnect: %v, want [a c] = %v", again, []string{a, c})
	}
	st2.none(t)
	st2.ack(a, "stored")
	st2.ack(c, "stored")
	waitPending(t, h.out, 0)
}

func TestRejectedTurnIsFinal(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "x"})
	st.turn(t)
	st.ack(id, "rejected")
	waitPending(t, h.out, 0)
	h.clk.tick(t, 11*time.Second)
	st.none(t)
}

func TestUnusableAcksAreIgnored(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "x"})
	st.turn(t)
	st.in <- json.RawMessage(`{"type":"turn.ack","turn_id":"` + id + `","status":"maybe"}`)
	st.in <- json.RawMessage(`{"type":"turn.ack","turn_id":"` + id + `"}`)
	st.in <- json.RawMessage(`{"type":"event","seq":1}`)
	st.ack(ops.NewUUIDv7(), "stored") // an id we never sent
	time.Sleep(30 * time.Millisecond)
	if got := pendingIDs(t, h.out); len(got) != 1 || got[0] != id {
		t.Fatalf("pending = %v, want the turn kept", got)
	}
}

func TestWriteFailureKeepsTheTurnAndReopens(t *testing.T) {
	h := startTurns(t)
	st1 := h.stream()
	st1.open(t)
	st1.failWrites(errors.New("broken pipe"))
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "x"})
	waitFor2(t, st1.wasReset)
	h.clk.tick(t, 2*time.Second)
	st2 := h.stream()
	st2.open(t)
	if tf := st2.turn(t); tf.TurnID != id {
		t.Fatalf("turn = %+v", tf)
	}
}

func TestOpenFailureIsRetriedAfterTheReopenDelay(t *testing.T) {
	h := &turnHarness{t: t, clk: newManualClock(), opens: make(chan *fakeEvStream, 4), done: make(chan struct{}), out: NewMemOutbox(0)}
	h.em = newEmitter(t, TurnConfig{Outbox: h.out, Clock: h.clk, ReopenDelay: 2 * time.Second})
	if _, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	failures := 1
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); <-h.done })
	go func() {
		h.em.serve(ctx, func(context.Context) (EventStream, error) {
			mu.Lock()
			defer mu.Unlock()
			if failures > 0 {
				failures--
				return nil, errors.New("no session")
			}
			st := newFakeEvStream()
			h.opens <- st
			return st, nil
		})
		close(h.done)
	}()
	h.clk.tick(t, 2*time.Second)
	st := h.stream()
	st.open(t)
	st.turn(t)
}

func TestLinkEndClosesTheTurnChannelGracefully(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	waitFor2(t, func() bool { return h.clk.waiters() > 0 })
	h.cancel()
	<-h.done
	if !st.wasClosed() || st.wasReset() {
		t.Fatalf("closed=%v reset=%v, want a graceful close", st.wasClosed(), st.wasReset())
	}
}

func TestEmitValidatesAndNormalisesTurns(t *testing.T) {
	em := newEmitter(t, TurnConfig{Clock: newManualClock(), Outbox: NewMemOutbox(2)})
	for name, tn := range map[string]PendingTurn{
		"no session":       {Text: "x"},
		"bad session name": {Session: "a:b", Text: "x"},
		"empty text":       {Session: "s"},
		"not UTF-8":        {Session: "s", Text: "\xff"},
		"bad turn id":      {Session: "s", Text: "x", TurnID: "nope"},
	} {
		if _, err := em.Emit(tn); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	id := ops.NewUUIDv7()
	for i := 0; i < 2; i++ { // the same turn_id twice is one turn
		if got, err := em.Emit(PendingTurn{MessageID: 7, Session: "s", Text: "x", TurnID: id}); err != nil || got != id {
			t.Fatalf("Emit with an id = %q, %v", got, err)
		}
	}
	p, _ := em.out.Pending()
	if len(p) != 1 || p[0].At.IsZero() {
		t.Fatalf("pending = %+v", p)
	}
	if _, err := em.Emit(PendingTurn{MessageID: 7, Session: "s", Text: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := em.Emit(PendingTurn{MessageID: 7, Session: "s", Text: "z"}); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("a full outbox = %v, want ErrOutboxFull", err)
	}
}

func TestLongTurnsAreCutAtACharacterBoundary(t *testing.T) {
	em := newEmitter(t, TurnConfig{Clock: newManualClock()})
	text := strings.Repeat("\u00e9", MaxTurnBytes) // 2 bytes each
	if _, err := em.Emit(PendingTurn{MessageID: 7, Session: "s", Text: text}); err != nil {
		t.Fatal(err)
	}
	p, _ := em.out.Pending()
	got := p[0].Text
	if len(got) > MaxTurnBytes || !strings.HasSuffix(got, TruncatedMarker) {
		t.Fatalf("len %d, suffix ok = %v", len(got), strings.HasSuffix(got, TruncatedMarker))
	}
	if body := strings.TrimSuffix(got, TruncatedMarker); strings.ContainsRune(body, '\uFFFD') || strings.Trim(body, "\u00e9") != "" {
		t.Fatal("the cut split a character")
	}
	short := strings.Repeat("x", MaxTurnBytes)
	if truncateTurn(short) != short {
		t.Fatal("a turn at the limit was cut")
	}
}

func TestMemOutboxRemoveAndOrder(t *testing.T) {
	o := NewMemOutbox(0)
	for _, id := range []string{"1", "2", "3"} {
		if err := o.Add(PendingTurn{TurnID: id}); err != nil {
			t.Fatal(err)
		}
	}
	_ = o.Remove("2")
	_ = o.Remove("nope")
	if got := pendingIDs(t, o); len(got) != 2 || got[0] != "1" || got[1] != "3" {
		t.Fatalf("pending = %v", got)
	}
}

// --- frame size (AD-3) ---

// frameSize is the size of the turn frame the emitter would send for t,
// measured with the real frame codec.
func frameSize(t testing.TB, p PendingTurn) (int, error) {
	t.Helper()
	var buf bytes.Buffer
	f := turnFrame{Type: FrameTurn, TurnID: p.TurnID, Session: p.Session, Text: p.Text, Time: ops.Timestamp(p.At)}
	err := proto.WriteFrame(&buf, proto.StreamFrameLimit, f)
	return buf.Len(), err
}

// progressSize is the size of the Hub's pipe_wait op.progress frame for p
// (the PipeTurn carries the same text plus a direction and a time).
func progressSize(t testing.TB, p PendingTurn) int {
	t.Helper()
	var buf bytes.Buffer
	data := map[string]any{"turn_id": p.TurnID, "session": p.Session, "direction": "received", "text": p.Text, "at": ops.Timestamp(p.At)}
	if err := proto.WriteFrame(&buf, proto.StreamFrameLimit, map[string]any{"type": proto.TypeOpProgress, "data": data}); err != nil {
		t.Fatalf("the Hub's progress frame does not fit: %v", err)
	}
	return buf.Len()
}

func TestEscapedTurnsStayUnderTheStreamFrameLimit(t *testing.T) {
	// 256 KiB of '<' encodes to six bytes each: 1,572,864 bytes of text.
	unit := map[string]string{
		"angle":          "<",
		"amp":            "&",
		"line separator": "\u2028",
		"control":        "\x01",
		"quote":          "\"",
		"backslash":      "\\",
		"newline":        "\n",
		"two byte":       "\u00e9",
		"three byte":     "\u65e5",
		"four byte":      "\U0001F600",
		"plain":          "x",
		"mixed":          "<a href=\"x\">&\u2028\x01\n\u00e9</a>",
	}
	for name, u := range unit {
		t.Run(name, func(t *testing.T) {
			em := newEmitter(t, TurnConfig{Clock: newManualClock()})
			id, err := em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: strings.Repeat(u, MaxTurnBytes/len(u)+1)})
			if err != nil {
				t.Fatal(err)
			}
			p, _ := em.out.Pending()
			if len(p) != 1 || p[0].TurnID != id {
				t.Fatalf("pending = %+v", p)
			}
			n, err := frameSize(t, p[0])
			if err != nil {
				t.Fatalf("the turn frame is refused by the codec: %v", err)
			}
			if n > proto.StreamFrameLimit {
				t.Fatalf("frame of %d bytes", n)
			}
			if progressSize(t, p[0]) > proto.StreamFrameLimit {
				t.Fatal("the Hub's progress frame is too large")
			}
			if !strings.HasSuffix(p[0].Text, TruncatedMarker) || !utf8.ValidString(p[0].Text) || len(p[0].Text) > MaxTurnBytes {
				t.Fatalf("text: len %d, marker %v, valid %v", len(p[0].Text), strings.HasSuffix(p[0].Text, TruncatedMarker), utf8.ValidString(p[0].Text))
			}
		})
	}
}

func TestEscapedTurnIsKeptWhenItFitsAndNeverGrowsPastTheLimit(t *testing.T) {
	// 100 KiB of '<' is 600 KiB encoded: fits, so it is not cut.
	text := strings.Repeat("<", 100<<10)
	if got := truncateTurn(text); got != text {
		t.Fatalf("a turn that fits was cut to %d bytes", len(got))
	}
	// Just over the encoded limit is cut, and what is left is the longest prefix that fits.
	over := strings.Repeat("<", MaxTurnEncodedBytes/6+10)
	got := truncateTurn(over)
	if !strings.HasSuffix(got, TruncatedMarker) || jsonStringLen(got) > MaxTurnEncodedBytes {
		t.Fatalf("len %d, encoded %d", len(got), jsonStringLen(got))
	}
	if jsonStringLen(got)+6 <= MaxTurnEncodedBytes {
		t.Fatalf("cut too early: encoded %d of %d", jsonStringLen(got), MaxTurnEncodedBytes)
	}
}

// limitedStream enforces the real frame limit on the writes of a fake stream.
type limitedStream struct{ *fakeEvStream }

func (s limitedStream) WriteFrame(v any) error {
	if err := proto.WriteFrame(&bytes.Buffer{}, proto.StreamFrameLimit, v); err != nil {
		return err
	}
	return s.fakeEvStream.WriteFrame(v)
}

func TestTooLargeFrameIsFinalForThatTurnAndDoesNotBlockTheOutbox(t *testing.T) {
	h := &turnHarness{t: t, clk: newManualClock(), opens: make(chan *fakeEvStream, 4), done: make(chan struct{}), out: NewMemOutbox(0)}
	var logs bytes.Buffer
	h.em = newEmitter(t, TurnConfig{Outbox: h.out, Clock: h.clk, ReopenDelay: 2 * time.Second, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	// A turn that bypasses Emit's cut (for example one left in a durable outbox by an older Worker).
	big := PendingTurn{TurnID: ops.NewUUIDv7(), MessageID: 7, Session: "minion-1", Text: strings.Repeat("<", 256<<10), At: time.Now()}
	if _, err := frameSize(t, big); !errors.Is(err, proto.ErrFrameTooLarge) {
		t.Fatalf("test setup: the big turn must be refused by the codec, got %v", err)
	}
	if err := h.out.Add(big); err != nil {
		t.Fatal(err)
	}
	next, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "after the big one"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { cancel(); <-h.done })
	go func() {
		h.em.serve(ctx, func(context.Context) (EventStream, error) {
			st := newFakeEvStream()
			h.opens <- st
			return limitedStream{st}, nil
		})
		close(h.done)
	}()
	st := h.stream()
	st.open(t)
	if tf := st.turn(t); tf.TurnID != next {
		t.Fatalf("the first frame is turn %s, want %s (the big one must be skipped)", tf.TurnID, next)
	}
	if st.wasReset() {
		t.Fatal("the stream was reset for a turn the codec refused")
	}
	st.ack(next, "stored")
	waitPending(t, h.out, 0)
	if strings.Contains(logs.String(), "<<<<") || !strings.Contains(logs.String(), big.TurnID) {
		t.Fatalf("the log must name the turn_id and never carry text: %q", logs.String()[:min(len(logs.String()), 300)])
	}
	select {
	case <-h.opens:
		t.Fatal("the stream was reopened")
	case <-time.After(50 * time.Millisecond):
	}
}

// --- share selection (A-9, AD-12) ---

func TestEmitRefusesATurnOfAnUnsharedSession(t *testing.T) {
	sh := &fakeShares{}
	sh.set("secret", false)
	h := startTurnsShares(t, sh)
	st := h.stream()
	st.open(t)
	if _, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "secret", Text: "private text"}); !errors.Is(err, ErrNotShared) {
		t.Fatalf("Emit of an unshared session = %v, want ErrNotShared", err)
	}
	if got := pendingIDs(t, h.out); len(got) != 0 {
		t.Fatalf("pending = %v", got)
	}
	id, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if tf := st.turn(t); tf.TurnID != id || tf.Session != "minion-1" {
		t.Fatalf("turn = %+v", tf)
	}
	st.none(t)
}

func TestTurnIsDroppedWhenItsSessionIsUnsharedBeforeTheSend(t *testing.T) {
	sh := &fakeShares{}
	out := NewMemOutbox(0)
	em := newEmitter(t, TurnConfig{Shares: sh, Outbox: out, Clock: newManualClock(), ReopenDelay: 2 * time.Second})
	if _, err := em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "queued while offline"}); err != nil {
		t.Fatal(err)
	}
	keep, err := em.Emit(PendingTurn{MessageID: 7, Session: "minion-2", Text: "stays shared"})
	if err != nil {
		t.Fatal(err)
	}
	sh.set("minion-1", false) // the owner unshares it before a Hub connection exists
	opens := make(chan *fakeEvStream, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		em.serve(ctx, func(context.Context) (EventStream, error) { st := newFakeEvStream(); opens <- st; return st, nil })
		close(done)
	}()
	st := <-opens
	st.open(t)
	if tf := st.turn(t); tf.TurnID != keep {
		t.Fatalf("turn = %+v, want only the shared session's turn", tf)
	}
	st.none(t)
	if got := pendingIDs(t, out); len(got) != 1 || got[0] != keep {
		t.Fatalf("pending = %v, want the unshared turn removed from the outbox", got)
	}
}

// revokingShares shares every session until allow lookups have been answered,
// then reports it unshared: the owner revokes while a batch is being written.
type revokingShares struct {
	mu    sync.Mutex
	allow int
}

func (r *revokingShares) Shared(context.Context, string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.allow <= 0 {
		return false, nil
	}
	r.allow--
	return true, nil
}

func (r *revokingShares) arm(n int) { r.mu.Lock(); r.allow = n; r.mu.Unlock() }

func TestSharingIsRecheckedBeforeEveryQueuedTurn(t *testing.T) {
	rs := &revokingShares{}
	rs.arm(2) // the two Emit checks
	out := NewMemOutbox(0)
	em := newEmitter(t, TurnConfig{Shares: rs, Outbox: out, Clock: newManualClock(), ReopenDelay: 2 * time.Second})
	first, err := em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "second"}); err != nil {
		t.Fatal(err)
	}
	rs.arm(1) // only the first send check passes; the share is revoked after the first write
	opens := make(chan *fakeEvStream, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		em.serve(ctx, func(context.Context) (EventStream, error) { st := newFakeEvStream(); opens <- st; return st, nil })
		close(done)
	}()
	st := <-opens
	st.open(t)
	if tf := st.turn(t); tf.TurnID != first {
		t.Fatalf("turn = %+v, want the first turn", tf)
	}
	st.none(t) // the second turn must not reach the Hub after the revocation
	if got := pendingIDs(t, out); len(got) != 1 || got[0] != first {
		t.Fatalf("pending = %v, want only the sent, unacked first turn", got)
	}
}

func TestShareLookupErrorFailsClosed(t *testing.T) {
	boom := errors.New("worker.db is locked")
	sh := &fakeShares{}
	// At Emit: the turn is not accepted.
	sh.failWith(boom)
	h := startTurnsShares(t, sh)
	st := h.stream()
	st.open(t)
	if _, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "x"}); !errors.Is(err, boom) {
		t.Fatalf("Emit with a failing lookup = %v", err)
	}
	if got := pendingIDs(t, h.out); len(got) != 0 {
		t.Fatalf("pending = %v", got)
	}
	// Before the send: nothing is sent while the lookup fails; the turn is kept and goes out once it works.
	sh.failWith(nil)
	id, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "held"})
	if err != nil {
		t.Fatal(err)
	}
	st.turn(t) // sent once, no ack yet
	st.ack(id, "stored")
	waitPending(t, h.out, 0)

	id2, err := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	st.turn(t)
	sh.failWith(boom)
	h.clk.tick(t, 11*time.Second) // the ack timeout wants to resend id2
	st.none(t)
	if got := pendingIDs(t, h.out); len(got) != 1 || got[0] != id2 {
		t.Fatalf("pending = %v, want the turn kept", got)
	}
	sh.failWith(nil)
	h.clk.tick(t, 11*time.Second)
	if tf := st.turn(t); tf.TurnID != id2 {
		t.Fatalf("turn = %+v", tf)
	}
}

func TestNewTurnEmitterNeedsAShareChecker(t *testing.T) {
	if _, err := NewTurnEmitter(TurnConfig{}); err == nil {
		t.Fatal("an emitter without a Shares port was accepted")
	}
}

func (s *fakeEvStream) ackBusy(id string, retryAfterMS int64) {
	b, _ := json.Marshal(map[string]any{"type": proto.TypeTurnAck, "turn_id": id, "status": "busy", "retry_after_ms": retryAfterMS})
	s.in <- b
	// A barrier: the reader has handled the busy answer when it has taken the
	// next frame.
	s.in <- json.RawMessage(`{"type":"noop"}`)
	for deadline := time.Now().Add(5 * time.Second); len(s.in) > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the sender wakes and recomputes its wait
}

// A turn answers a delivery of the Hub: it carries the message_id the Hub gave
// it, and a turn without one is not queued (the Hub could never store it).
func TestATurnNamesTheDeliveryItAnswers(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	if _, err := h.em.Emit(PendingTurn{Session: "minion-1", Text: "no delivery"}); err == nil {
		t.Fatal("a turn without a message_id was queued")
	}
	if p, _ := h.out.Pending(); len(p) != 0 {
		t.Fatalf("outbox = %+v", p)
	}
	id, err := h.em.Emit(PendingTurn{MessageID: 4242, Session: "minion-1", Text: "reply"})
	if err != nil {
		t.Fatal(err)
	}
	if tf := st.turn(t); tf.TurnID != id || tf.MessageID != 4242 {
		t.Fatalf("turn = %+v, want message_id 4242", tf)
	}
}

// busy is not final and not an acknowledgement: the turn stays in the outbox
// and is sent again only after the time the Hub named, not at the ack timeout.
func TestABusyAnswerKeepsTheTurnAndHoldsItBackForRetryAfter(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "one"})
	st.turn(t)
	st.ackBusy(id, 60_000)
	waitFor2(t, func() bool { return h.clk.waiters() > 0 })
	h.clk.tick(t, 11*time.Second) // the ack timeout passes
	st.none(t)
	h.clk.tick(t, 40*time.Second) // 51 s: still within retry_after_ms
	st.none(t)
	if got := pendingIDs(t, h.out); len(got) != 1 || got[0] != id {
		t.Fatalf("pending = %v: a busy turn is not dropped", got)
	}
	h.clk.tick(t, 10*time.Second) // 61 s
	if tf := st.turn(t); tf.TurnID != id {
		t.Fatalf("turn after retry_after = %+v", tf)
	}
	st.ack(id, "stored")
	waitPending(t, h.out, 0)
}

// The wait the Hub asks for is bounded below (a Hub that says 0 does not make
// a busy loop) and above (a Hub that says a day does not hold a turn for it).
func TestBusyWaitIsBounded(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "one"})
	st.turn(t)
	st.ackBusy(id, 0)
	waitFor2(t, func() bool { return h.clk.waiters() > 0 })
	h.clk.tick(t, 500*time.Millisecond)
	st.none(t) // not before MinBusyWait
	h.clk.tick(t, 600*time.Millisecond)
	if tf := st.turn(t); tf.TurnID != id {
		t.Fatalf("turn = %+v", tf)
	}
	st.ackBusy(id, int64(24*time.Hour/time.Millisecond))
	waitFor2(t, func() bool { return h.clk.waiters() > 0 })
	h.clk.tick(t, MaxBusyWait+time.Second)
	if tf := st.turn(t); tf.TurnID != id {
		t.Fatalf("turn after the longest wait = %+v", tf)
	}
}

// A final rejection drops the turn and it is never sent again; a Hub that stays
// busy does not keep a turn for ever.
func TestARejectedTurnIsDroppedAndABusyHubDoesNotHoldATurnForEver(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	rej, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "rejected"})
	st.turn(t)
	st.ack(rej, "rejected")
	waitPending(t, h.out, 0)
	h.clk.tick(t, 30*time.Second)
	st.none(t)

	id, _ := h.em.Emit(PendingTurn{MessageID: 8, Session: "minion-1", Text: "busy for ever"})
	st.turn(t)
	for i := 0; i < 5; i++ {
		st.ackBusy(id, 60_000)
		waitFor2(t, func() bool { return h.clk.waiters() > 0 })
		h.clk.tick(t, 61*time.Second)
		if p, _ := h.out.Pending(); len(p) == 0 {
			t.Fatalf("dropped after %d busy answers, long before MaxAge", i+1)
		}
		st.turn(t)
	}
	h.clk.tick(t, DefaultTurnMaxAge)
	waitPending(t, h.out, 0)
	st.none(t)
}

// A backlog goes out a window at a time: with 30 turns waiting, 8 are sent and
// no more until the Hub answers, and every answer makes room for the next one.
func TestABacklogGoesOutAWindowAtATime(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	var ids []string
	for i := 0; i < 30; i++ {
		id, err := h.em.Emit(PendingTurn{MessageID: int64(10 + i), Session: "minion-1", Text: fmt.Sprintf("t%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var sent []string
	for i := 0; i < DefaultMaxInFlight; i++ {
		sent = append(sent, st.turn(t).TurnID)
	}
	st.none(t) // the window is full
	if fmt.Sprint(sent) != fmt.Sprint(ids[:DefaultMaxInFlight]) {
		t.Fatalf("sent %v, want the oldest %d in order", sent, DefaultMaxInFlight)
	}
	st.ack(sent[0], "stored")
	if got := st.turn(t).TurnID; got != ids[DefaultMaxInFlight] {
		t.Fatalf("after one answer the next turn is %s, want %s", got, ids[DefaultMaxInFlight])
	}
	st.none(t)
	for _, id := range sent[1:] {
		st.ack(id, "stored")
	}
	for i := 0; i < DefaultMaxInFlight-1; i++ { // one slot is taken by the turn sent above
		st.turn(t)
	}
	st.none(t)
}

// A busy answer holds the whole channel, not only its turn, and the hold is the
// emitter's: a stream that is reset and opened again does not send the backlog
// at once. After the hold one turn goes out to find out whether there is room,
// and the window widens by one for every turn the Hub takes.
func TestABusyHoldSurvivesAResetAndTheWholeChannelWaits(t *testing.T) {
	h := startTurns(t)
	st1 := h.stream()
	st1.open(t)
	var ids []string
	for i := 0; i < 20; i++ {
		id, _ := h.em.Emit(PendingTurn{MessageID: int64(10 + i), Session: "minion-1", Text: fmt.Sprintf("t%d", i)})
		ids = append(ids, id)
	}
	for i := 0; i < DefaultMaxInFlight; i++ {
		st1.turn(t)
	}
	// The Hub is out of capacity: it answers busy to the window, for a minute.
	for _, id := range ids[:DefaultMaxInFlight] {
		st1.ackBusy(id, 60_000)
	}
	st1.none(t)
	pending := len(pendingIDs(t, h.out))
	if pending != 20 {
		t.Fatalf("%d turns pending, a busy answer must keep every turn", pending)
	}

	// The stream is reset and opened again after the reopen delay: nothing is
	// sent, not even the turns that never had a busy answer.
	st1.endHub()
	waitFor2(t, st1.wasReset)
	h.clk.tick(t, 2*time.Second)
	st2 := h.stream()
	st2.open(t)
	st2.none(t)
	h.clk.tick(t, 30*time.Second) // 32 s of 60
	st2.none(t)

	// Another reset in the middle of the hold changes nothing.
	st2.endHub()
	waitFor2(t, st2.wasReset)
	h.clk.tick(t, 2*time.Second)
	st3 := h.stream()
	st3.open(t)
	st3.none(t)

	// The hold is over: exactly one probe.
	h.clk.tick(t, 40*time.Second)
	probe := st3.turn(t)
	if probe.TurnID != ids[0] {
		t.Fatalf("probe = %s, want the oldest turn %s", probe.TurnID, ids[0])
	}
	st3.none(t)
	// The Hub takes it: the window widens by one, two turns follow.
	st3.ack(probe.TurnID, "stored")
	st3.turn(t)
	st3.turn(t)
	st3.none(t)
}

// The holds are bounded by the outbox: they end with their turn, whatever ends it.
func TestBusyHoldsEndWithTheirTurns(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	var ids []string
	for i := 0; i < 3; i++ {
		id, _ := h.em.Emit(PendingTurn{MessageID: int64(10 + i), Session: "minion-1", Text: fmt.Sprintf("t%d", i)})
		ids = append(ids, id)
	}
	for range ids {
		st.turn(t)
	}
	for _, id := range ids {
		st.ackBusy(id, 600_000)
	}
	held := func() int { h.em.holdMu.Lock(); defer h.em.holdMu.Unlock(); return len(h.em.busyUntil) }
	if held() != 3 {
		t.Fatalf("%d holds, want 3", held())
	}
	st.ack(ids[0], "rejected") // final: the turn and its hold are gone
	st.ack(ids[1], "stored")   // the Hub took it after all
	waitFor2(t, func() bool { return held() == 1 })
	if err := h.out.Remove(ids[2]); err != nil { // the turn is dropped (too old, or no longer shared)
		t.Fatal(err)
	}
	h.clk.tick(t, 11*time.Minute) // the sender looks at the outbox again when the hold would end
	waitFor2(t, func() bool { return held() == 0 })
}

// A hold is never longer than the bound, whatever retry_after the Hub asks.
func TestABusyHoldIsBoundedAboveAndBelow(t *testing.T) {
	h := startTurns(t)
	st := h.stream()
	st.open(t)
	id, _ := h.em.Emit(PendingTurn{MessageID: 7, Session: "minion-1", Text: "x"})
	st.turn(t)
	st.ackBusy(id, int64(7*24*time.Hour/time.Millisecond))
	h.em.holdMu.Lock()
	left := h.em.chanHold.Sub(h.clk.Now())
	h.em.holdMu.Unlock()
	if left > MaxBusyWait {
		t.Fatalf("the channel is held for %s, more than the bound %s", left, MaxBusyWait)
	}
}
