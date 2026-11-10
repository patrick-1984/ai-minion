package pipe

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var (
	ctx   = context.Background()
	alice = Peer{WorkerID: "w1", SessionName: "minion-1"}
	bob   = Peer{WorkerID: "w1", SessionName: "minion-2"}
)

func newTestBroker(t *testing.T, budget int) (*Broker, *memStore, *fakeClock) {
	t.Helper()
	st, clk := newMemStore(), newClock()
	b, err := NewBroker(Config{Store: st, Now: clk.Now, BudgetPerDay: budget})
	if err != nil {
		t.Fatal(err)
	}
	return b, st, clk
}

// wantCode fails unless err carries the code want. It returns RetryAfter of
// the *Error (zero if none) so no call site drops an error-typed result.
func wantCode(t *testing.T, err error, want Code) time.Duration {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got nil error", want)
	}
	if got := CodeOf(err); got != want {
		t.Fatalf("code = %s, want %s (%v)", got, want, err)
	}
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

func TestNewBrokerRequiresStore(t *testing.T) {
	if _, err := NewBroker(Config{}); err == nil {
		t.Fatal("want error without store")
	}
	b, _, _ := newTestBroker(t, 0)
	if b.BudgetPerDay() != DefaultBudgetPerDay || DefaultBudgetPerDay != 200 {
		t.Fatalf("default budget = %d", b.BudgetPerDay())
	}
}

func TestSendStoresMessage(t *testing.T) {
	b, _, clk := newTestBroker(t, 0)
	m, err := b.Send(ctx, alice, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != 1 || m.Peer != alice || m.Text != "hello" || !m.At.Equal(clk.Now()) {
		t.Fatalf("message = %+v", m)
	}
	got, _ := b.Messages(ctx, alice, 0, 0)
	if len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("messages = %+v", got)
	}
	if other, _ := b.Messages(ctx, bob, 0, 0); len(other) != 0 {
		t.Fatalf("bob sees %+v", other)
	}
}

func TestSendValidation(t *testing.T) {
	b, st, _ := newTestBroker(t, 0)
	for _, tc := range []struct {
		peer Peer
		text string
	}{
		{Peer{}, "x"}, {Peer{WorkerID: "w"}, "x"}, {Peer{SessionName: "s"}, "x"}, {alice, ""},
	} {
		_, err := b.Send(ctx, tc.peer, tc.text)
		wantCode(t, err, CodeInvalidArgument)
	}
	if len(st.messages) != 0 {
		t.Fatal("invalid sends were stored")
	}
}

func TestMessageSizeLimit(t *testing.T) {
	b, st, _ := newTestBroker(t, 0)
	if _, err := b.Send(ctx, alice, strings.Repeat("a", 8192)); err != nil {
		t.Fatalf("exactly 8 KiB must pass: %v", err)
	}
	_, err := b.Send(ctx, alice, strings.Repeat("a", 8193))
	wantCode(t, err, CodeMessageTooLarge)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatal("errors.Is sentinel")
	}
	// The limit is in bytes, not runes: 4097 two-byte runes are 8194 bytes.
	_, err = b.Send(ctx, alice, strings.Repeat("\u00e9", 4097))
	wantCode(t, err, CodeMessageTooLarge)
	if len(st.messages) != 1 {
		t.Fatalf("stored %d messages, want 1", len(st.messages))
	}
	// A rejected oversized message uses no rate token.
	b2, _, _ := newTestBroker(t, 0)
	for i := 0; i < 10; i++ {
		b2.Send(ctx, alice, strings.Repeat("a", 9000)) //nolint:errcheck
	}
	for i := 0; i < SendBurst; i++ {
		if _, err := b2.Send(ctx, alice, "ok"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
}

func TestRateLimitBurstAndRefill(t *testing.T) {
	b, st, clk := newTestBroker(t, 1000)
	for i := 0; i < SendBurst; i++ {
		if _, err := b.Send(ctx, alice, "m"); err != nil {
			t.Fatalf("burst send %d: %v", i, err)
		}
	}
	_, err := b.Send(ctx, alice, "m")
	if ra := wantCode(t, err, CodePipeRateLimited); ra != time.Second {
		t.Fatalf("RetryAfter = %v, want 1s", ra)
	}
	if len(st.messages) != SendBurst {
		t.Fatalf("rejected message was stored or queued: %d", len(st.messages))
	}
	// Other peers are independent.
	if _, err := b.Send(ctx, bob, "m"); err != nil {
		t.Fatalf("bob: %v", err)
	}
	// Nothing is delayed or queued: after 1s exactly one more is accepted.
	clk.Add(time.Second)
	if _, err := b.Send(ctx, alice, "m"); err != nil {
		t.Fatalf("after refill: %v", err)
	}
	_, err = b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeRateLimited)
	// Half a second later the retry hint shrinks accordingly.
	clk.Add(500 * time.Millisecond)
	_, err = b.Send(ctx, alice, "m")
	if ra := wantCode(t, err, CodePipeRateLimited); ra != 500*time.Millisecond {
		t.Fatalf("RetryAfter = %v", ra)
	}
	// A long pause refills only up to the burst.
	clk.Add(time.Hour)
	for i := 0; i < SendBurst; i++ {
		if _, err := b.Send(ctx, alice, "m"); err != nil {
			t.Fatalf("after pause %d: %v", i, err)
		}
	}
	_, err = b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeRateLimited)
}

func TestRateLimitHourly(t *testing.T) {
	b, st, clk := newTestBroker(t, 100000)
	start := clk.Now()
	// One message per second for 120 seconds fills the hour window.
	for i := 0; i < SendsPerHour; i++ {
		if _, err := b.Send(ctx, alice, "m"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		clk.Add(time.Second)
	}
	_, err := b.Send(ctx, alice, "m")
	ra := wantCode(t, err, CodePipeRateLimited)
	// The oldest send (at start) leaves the window at start+1h.
	if want := start.Add(time.Hour).Sub(clk.Now()); ra != want {
		t.Fatalf("RetryAfter = %v, want %v", ra, want)
	}
	if len(st.messages) != SendsPerHour {
		t.Fatalf("stored %d", len(st.messages))
	}
	// Still blocked later in the hour even though the bucket is full.
	clk.Add(30 * time.Minute)
	_, err = b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeRateLimited)
	// Once the oldest send is older than one hour, one more is accepted.
	clk.Add(time.Hour - 30*time.Minute - 120*time.Second + time.Second)
	if _, err := b.Send(ctx, alice, "m"); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestBudget(t *testing.T) {
	b, st, clk := newTestBroker(t, 10)
	for i := 0; i < 10; i++ {
		if _, err := b.Send(ctx, alice, "m"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		clk.Add(2 * time.Second)
	}
	_, err := b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeBudgetExceeded)
	if !errors.Is(err, ErrPipeBudgetExceeded) {
		t.Fatal("sentinel")
	}
	if len(st.messages) != 10 {
		t.Fatalf("stored %d", len(st.messages))
	}
	// Per peer: bob has his own budget.
	if _, err := b.Send(ctx, bob, "m"); err != nil {
		t.Fatalf("bob: %v", err)
	}
	// After 24 h from the first message the budget frees up again.
	clk.Add(24 * time.Hour)
	if _, err := b.Send(ctx, alice, "m"); err != nil {
		t.Fatalf("after 24h: %v", err)
	}
}

func TestBudgetDefault200(t *testing.T) {
	st, clk := newMemStore(), newClock()
	b, _ := NewBroker(Config{Store: st, Now: clk.Now})
	// 120 per hour is the rate cap, so spread 200 sends over several hours.
	for i := 0; i < 200; i++ {
		if _, err := b.Send(ctx, alice, "m"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		clk.Add(2 * time.Minute) // 30/h, 200 sends take 6h40
	}
	_, err := b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeBudgetExceeded)
}

func TestRejectedSendDoesNotUseBudget(t *testing.T) {
	b, st, _ := newTestBroker(t, 6)
	for i := 0; i < SendBurst; i++ {
		b.Send(ctx, alice, "m") //nolint:errcheck
	}
	for i := 0; i < 50; i++ { // all rate limited
		_, err := b.Send(ctx, alice, "m")
		wantCode(t, err, CodePipeRateLimited)
	}
	if len(st.messages) != SendBurst {
		t.Fatalf("stored %d", len(st.messages))
	}
	b.limiter = newSendLimiter() // reset the rate state only
	if _, err := b.Send(ctx, alice, "m"); err != nil {
		t.Fatalf("budget wrongly consumed by rejections: %v", err)
	}
}

func TestSendConcurrentRespectsLimits(t *testing.T) {
	b, st, _ := newTestBroker(t, 1000)
	var wg sync.WaitGroup
	var ok, limited int
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Send(ctx, alice, "m")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if CodeOf(err) == CodePipeRateLimited {
				limited++
			}
		}()
	}
	wg.Wait()
	if ok != SendBurst || limited != 50-SendBurst || len(st.messages) != SendBurst {
		t.Fatalf("ok=%d limited=%d stored=%d", ok, limited, len(st.messages))
	}
}

func TestRateLimiterBoundedMemory(t *testing.T) {
	l := newSendLimiter()
	clk := newClock()
	peer := func(i int) Peer { return Peer{WorkerID: "w", SessionName: "s" + strconv.Itoa(i)} }
	for i := 0; i < maxTrackedPeers; i++ {
		if err := l.take(peer(i), clk.Now()); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Millisecond)
	}
	// Every tracked peer still enforces a limit: a new peer is rejected
	// instead of evicting one, and the memory stays bounded.
	err := l.take(peer(maxTrackedPeers), clk.Now())
	if err == nil || err.Code != CodePipeRateLimited || err.RetryAfter <= 0 {
		t.Fatalf("new peer at capacity: %v", err)
	}
	if len(l.peers) != maxTrackedPeers {
		t.Fatalf("peers = %d, cap %d", len(l.peers), maxTrackedPeers)
	}
	// After the hour window the idle peers are dropped and a new peer fits.
	clk.Add(time.Hour + time.Minute)
	if err := l.take(peer(maxTrackedPeers), clk.Now()); err != nil {
		t.Fatal(err)
	}
	if len(l.peers) > maxTrackedPeers {
		t.Fatalf("peers = %d", len(l.peers))
	}
}

// Capacity pressure must never reset the limits of an active peer.
func TestRateLimiterEvictionKeepsLimits(t *testing.T) {
	l := newSendLimiter()
	clk := newClock()
	for i := 0; i < SendBurst; i++ {
		if err := l.take(alice, clk.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < maxTrackedPeers+10; i++ {
		// Rejections at capacity are expected and ignored.
		_ = l.take(Peer{WorkerID: "o", SessionName: "s" + strconv.Itoa(i)}, clk.Now())
	}
	if err := l.take(alice, clk.Now()); err == nil || err.Code != CodePipeRateLimited {
		t.Fatalf("alice got a fresh burst after capacity pressure: %v", err)
	}
	if len(l.peers) > maxTrackedPeers {
		t.Fatalf("peers = %d", len(l.peers))
	}
}

// RetryAfter is the larger of the hour wait and the token wait.
func TestRetryAfterIsLargerOfBothWaits(t *testing.T) {
	l := newSendLimiter()
	clk := newClock()
	st := &peerState{credit: SendInterval / 10, last: clk.Now()} // 0.1 token
	for i := 0; i < SendsPerHour; i++ {
		st.sends = append(st.sends, clk.Now().Add(-HourWindow+100*time.Millisecond+time.Duration(i)))
	}
	l.peers[alice] = st
	err := l.take(alice, clk.Now())
	if err == nil || err.Code != CodePipeRateLimited {
		t.Fatalf("want rate limited, got %v", err)
	}
	// The oldest send leaves in 100ms, but 0.9s of refill is still needed.
	if err.RetryAfter != 900*time.Millisecond {
		t.Fatalf("RetryAfter = %v, want 900ms", err.RetryAfter)
	}
	// Retrying at RetryAfter is accepted.
	clk.Add(err.RetryAfter)
	if err := l.take(alice, clk.Now()); err != nil {
		t.Fatalf("retry at RetryAfter rejected: %v", err)
	}
	// And the reverse: the hour wait dominates when it is longer.
	st2 := &peerState{credit: 0, last: clk.Now()}
	for i := 0; i < SendsPerHour; i++ {
		st2.sends = append(st2.sends, clk.Now().Add(-time.Minute))
	}
	l.peers[bob] = st2
	if err := l.take(bob, clk.Now()); err == nil || err.RetryAfter != HourWindow-time.Minute {
		t.Fatalf("hour wait: %v", err)
	}
}

func TestClockStepBackMintsNoTokens(t *testing.T) {
	b, _, clk := newTestBroker(t, 1000)
	for i := 0; i < SendBurst; i++ {
		b.Send(ctx, alice, "m") //nolint:errcheck
	}
	clk.Add(-time.Hour)
	_, err := b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeRateLimited)
}

func uuid7(t *testing.T, clk *fakeClock) string {
	t.Helper()
	id, err := NewTurnID(clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCommitTurnIdempotent(t *testing.T) {
	b, st, clk := newTestBroker(t, 0)
	id := uuid7(t, clk)
	turn := Turn{TurnID: id, Peer: alice, Text: "reply", At: clk.Now()}
	for i := 0; i < 2; i++ {
		ack, err := b.CommitTurn(ctx, turn)
		if err != nil {
			t.Fatal(err)
		}
		if ack.Status != AckStored || ack.TurnID != id || ack.Reason != "" {
			t.Fatalf("ack %d = %+v", i, ack)
		}
	}
	if len(st.turns) != 1 {
		t.Fatalf("stored %d turns, want 1", len(st.turns))
	}
	// A retry with a different text does not overwrite the stored turn.
	turn.Text = "other"
	if ack, err := b.CommitTurn(ctx, turn); err != nil || ack.Status != AckStored {
		t.Fatalf("ack = %+v err = %v", ack, err)
	}
	if st.turns[0].Text != "reply" {
		t.Fatalf("stored turn changed: %q", st.turns[0].Text)
	}
}

func TestCommitTurnIgnoresSendLimits(t *testing.T) {
	b, st, clk := newTestBroker(t, 1)
	for i := 0; i < 30; i++ {
		clk.Add(time.Millisecond)
		if ack, err := b.CommitTurn(ctx, Turn{TurnID: uuid7(t, clk), Peer: alice, Text: "r"}); err != nil || ack.Status != AckStored {
			t.Fatalf("commit %d: %+v %v", i, ack, err)
		}
	}
	if len(st.turns) != 30 || len(st.messages) != 0 {
		t.Fatalf("turns=%d messages=%d", len(st.turns), len(st.messages))
	}
}

func TestCommitTurnDefaultsTimestamp(t *testing.T) {
	b, st, clk := newTestBroker(t, 0)
	b.CommitTurn(ctx, Turn{TurnID: uuid7(t, clk), Peer: alice, Text: "r"}) //nolint:errcheck
	if !st.turns[0].At.Equal(clk.Now()) {
		t.Fatalf("At = %v", st.turns[0].At)
	}
}

func TestCommitTurnRejectsInvalid(t *testing.T) {
	b, st, clk := newTestBroker(t, 0)
	v4 := "3f2b8c1e-5d4a-4b6c-9e7f-0a1b2c3d4e5f"
	for _, tc := range []Turn{
		{TurnID: "", Peer: alice, Text: "x"},
		{TurnID: v4, Peer: alice, Text: "x"},
		{TurnID: uuid7(t, clk), Peer: Peer{WorkerID: "w"}, Text: "x"},
	} {
		ack, err := b.CommitTurn(ctx, tc)
		wantCode(t, err, CodeInvalidArgument)
		if ack.Status != AckRejected || ack.Reason == "" {
			t.Fatalf("ack = %+v", ack)
		}
	}
	if len(st.turns) != 0 {
		t.Fatal("invalid turn stored")
	}
}

func TestTurnsReadInCommitOrder(t *testing.T) {
	b, _, clk := newTestBroker(t, 0)
	var ids []string
	for i := 0; i < 4; i++ {
		clk.Add(time.Millisecond)
		id := uuid7(t, clk)
		ids = append(ids, id)
		peer := alice
		if i == 2 {
			peer = bob
		}
		b.CommitTurn(ctx, Turn{TurnID: id, Peer: peer, Text: id}) //nolint:errcheck
	}
	all, err := b.Turns(ctx, alice, "", 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %v %v", all, err)
	}
	after, err := b.Turns(ctx, alice, ids[0], 0)
	if err != nil || len(after) != 2 || after[0].TurnID != ids[1] || after[1].TurnID != ids[3] {
		t.Fatalf("after = %v %v", after, err)
	}
	if lim, _ := b.Turns(ctx, alice, "", 1); len(lim) != 1 {
		t.Fatalf("limit: %v", lim)
	}
	if rest, err := b.Turns(ctx, alice, ids[3], 0); err != nil || len(rest) != 0 {
		t.Fatalf("rest = %v %v", rest, err)
	}
	unknown := uuid7(t, clk)
	_, err = b.Turns(ctx, alice, unknown, 0)
	wantCode(t, err, CodeNotFound)
	_, err = b.Turns(ctx, alice, "nonsense", 0)
	wantCode(t, err, CodeInvalidArgument)
}

func TestPrune(t *testing.T) {
	b, st, clk := newTestBroker(t, 1000)
	old := clk.Now()
	b.Send(ctx, alice, "old")                                                         //nolint:errcheck
	b.CommitTurn(ctx, Turn{TurnID: uuid7(t, clk), Peer: alice, Text: "old", At: old}) //nolint:errcheck

	clk.Add(6 * 24 * time.Hour)
	mid := clk.Now()
	b.Send(ctx, alice, "mid")                                                         //nolint:errcheck
	b.CommitTurn(ctx, Turn{TurnID: uuid7(t, clk), Peer: alice, Text: "mid", At: mid}) //nolint:errcheck

	// 7 days minus a moment after the first: nothing is older than retention.
	res, err := b.Prune(ctx, old.Add(Retention))
	if err != nil || res != (PruneResult{}) {
		t.Fatalf("prune at boundary = %+v %v", res, err)
	}
	// One nanosecond later the first message and turn are older than 7 days.
	res, err = b.Prune(ctx, old.Add(Retention+time.Nanosecond))
	if err != nil || res != (PruneResult{Messages: 1, Turns: 1}) {
		t.Fatalf("prune = %+v %v", res, err)
	}
	if len(st.messages) != 1 || st.messages[0].Text != "mid" || len(st.turns) != 1 || st.turns[0].Text != "mid" {
		t.Fatalf("remaining: %+v %+v", st.messages, st.turns)
	}
	// Everything goes after another 7 days.
	res, _ = b.Prune(ctx, clk.Now().Add(Retention+time.Second))
	if res != (PruneResult{Messages: 1, Turns: 1}) || len(st.messages)+len(st.turns) != 0 {
		t.Fatalf("final prune = %+v", res)
	}
	if Retention != 7*24*time.Hour {
		t.Fatal("retention constant")
	}
}

func TestBrokerNeverReplies(t *testing.T) {
	// The broker has no outbound path: sending stores exactly one message and
	// commits store exactly one turn; nothing else appears in the store.
	b, st, clk := newTestBroker(t, 0)
	b.Send(ctx, alice, "ping")                                                //nolint:errcheck
	b.CommitTurn(ctx, Turn{TurnID: uuid7(t, clk), Peer: alice, Text: "pong"}) //nolint:errcheck
	if len(st.messages) != 1 || len(st.turns) != 1 {
		t.Fatalf("messages=%d turns=%d", len(st.messages), len(st.turns))
	}
}

func TestErrorHelpers(t *testing.T) {
	e := newError(CodeMessageTooLarge, "x %d", 1)
	if e.Error() != "message_too_large: x 1" || (&Error{Code: CodeNotFound}).Error() != "not_found" {
		t.Fatalf("Error() = %q", e.Error())
	}
	if !errors.Is(e, ErrMessageTooLarge) || errors.Is(e, ErrNotFound) {
		t.Fatal("Is")
	}
	if CodeOf(errors.New("plain")) != CodeInternal {
		t.Fatal("CodeOf plain")
	}
}

// A live budget is asked on every send; a failing or empty answer keeps the
// last usable one, so that a broken setting store never lifts the limit.
func TestBrokerBudgetFuncIsLiveAndFailsToTheLastBudget(t *testing.T) {
	ctx := context.Background()
	budget, fail := 2, false
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	b, err := NewBroker(Config{Store: newMemStore(), Now: func() time.Time { return clock },
		BudgetFunc: func() (int, error) {
			if fail {
				return 0, errors.New("setting store down")
			}
			return budget, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	peer := Peer{WorkerID: "w", SessionName: "s"}
	send := func() error {
		clock = clock.Add(2 * time.Second) // inside the rate limit
		_, err := b.Send(ctx, peer, "x")
		return err
	}
	if err := send(); err != nil {
		t.Fatal(err)
	}
	if err := send(); err != nil {
		t.Fatal(err)
	}
	if err := send(); !errors.Is(err, ErrPipeBudgetExceeded) {
		t.Fatalf("third send = %v, want the budget of 2", err)
	}
	budget = 3 // raised in the panel
	if err := send(); err != nil {
		t.Fatalf("send after raising the budget = %v", err)
	}
	fail = true // the setting store fails: the last answer (3) still holds
	if err := send(); !errors.Is(err, ErrPipeBudgetExceeded) || b.BudgetPerDay() != 3 {
		t.Fatalf("send with a failing setting = %v, budget %d", err, b.BudgetPerDay())
	}
}
