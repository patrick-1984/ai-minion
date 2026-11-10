package pipe

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

// failStore wraps memStore with injectable errors per method. A nil error
// means the call goes through to the in-memory store.
type failStore struct {
	*memStore
	appendErr, countErr, putErr, deleteErr error
}

func (s *failStore) AppendMessage(c context.Context, m Message) (Message, error) {
	if s.appendErr != nil {
		return Message{}, s.appendErr
	}
	return s.memStore.AppendMessage(c, m)
}

func (s *failStore) CountMessagesSince(c context.Context, p Peer, since time.Time) (int, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.memStore.CountMessagesSince(c, p, since)
}

func (s *failStore) PutTurn(c context.Context, t Turn) (bool, error) {
	if s.putErr != nil {
		return false, s.putErr
	}
	return s.memStore.PutTurn(c, t)
}

func (s *failStore) DeleteBefore(c context.Context, cutoff time.Time) (int, int, error) {
	if s.deleteErr != nil {
		return 0, 0, s.deleteErr
	}
	return s.memStore.DeleteBefore(c, cutoff)
}

func newFailBroker(t *testing.T) (*Broker, *failStore, *fakeClock) {
	t.Helper()
	fs, clk := &failStore{memStore: newMemStore()}, newClock()
	b, err := NewBroker(Config{Store: fs, Now: clk.Now, BudgetPerDay: 100000})
	if err != nil {
		t.Fatal(err)
	}
	return b, fs, clk
}

func TestSendAppendFailure(t *testing.T) {
	b, fs, _ := newFailBroker(t)
	fs.appendErr = errBoom
	m, err := b.Send(ctx, alice, "hello")
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if got := CodeOf(err); got != CodeInternal {
		t.Fatalf("code = %s, want %s", got, CodeInternal)
	}
	if m != (Message{}) {
		t.Fatalf("message = %+v, want zero", m)
	}
	if len(fs.messages) != 0 {
		t.Fatal("message stored")
	}
}

func TestSendCountFailure(t *testing.T) {
	b, fs, _ := newFailBroker(t)
	fs.countErr = errBoom
	m, err := b.Send(ctx, alice, "hello")
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if m != (Message{}) || len(fs.messages) != 0 {
		t.Fatalf("stored on budget failure: %+v %d", m, len(fs.messages))
	}
}

// A failed write must not consume burst or hourly quota.
func TestFailedAppendDoesNotUseQuota(t *testing.T) {
	b, fs, clk := newFailBroker(t)
	fs.appendErr = errBoom
	for i := 0; i < 3*SendsPerHour; i++ {
		if _, err := b.Send(ctx, alice, "m"); err == nil {
			t.Fatal("want error")
		}
		clk.Add(time.Second)
	}
	fs.appendErr = nil
	for i := 0; i < SendBurst; i++ {
		if _, err := b.Send(ctx, alice, "m"); err != nil {
			t.Fatalf("burst send %d after recovery: %v", i, err)
		}
	}
	_, err := b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeRateLimited)
	if len(fs.messages) != SendBurst {
		t.Fatalf("stored %d", len(fs.messages))
	}
	// The hourly window holds only the real sends: fill it up to the cap.
	for len(fs.messages) < SendsPerHour {
		clk.Add(time.Second)
		if _, err := b.Send(ctx, alice, "m"); err != nil {
			t.Fatalf("send %d: %v", len(fs.messages), err)
		}
	}
	clk.Add(time.Second)
	_, err = b.Send(ctx, alice, "m")
	wantCode(t, err, CodePipeRateLimited)
}

func TestCommitTurnPutFailure(t *testing.T) {
	b, fs, clk := newFailBroker(t)
	fs.putErr = errBoom
	id := uuid7(t, clk)
	ack, err := b.CommitTurn(ctx, Turn{TurnID: id, Peer: alice, Text: "r"})
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if ack.Status == AckStored {
		t.Fatalf("ack = %+v: an ack means stored", ack)
	}
	if len(fs.turns) != 0 {
		t.Fatal("turn stored")
	}
	// The resend after recovery is acked as stored.
	fs.putErr = nil
	ack, err = b.CommitTurn(ctx, Turn{TurnID: id, Peer: alice, Text: "r"})
	if err != nil || ack.Status != AckStored || len(fs.turns) != 1 {
		t.Fatalf("resend: %+v %v", ack, err)
	}
}

func TestPruneDeleteFailure(t *testing.T) {
	b, fs, clk := newFailBroker(t)
	fs.deleteErr = errBoom
	res, err := b.Prune(ctx, clk.Now())
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if res != (PruneResult{}) {
		t.Fatalf("result = %+v", res)
	}
}

// A client-supplied timestamp cannot keep a turn beyond the retention.
func TestCommitTurnClampsFutureTimestamp(t *testing.T) {
	b, st, clk := newTestBroker(t, 0)
	commit := clk.Now()
	future := commit.Add(10 * 365 * 24 * time.Hour)
	ack, err := b.CommitTurn(ctx, Turn{TurnID: uuid7(t, clk), Peer: alice, Text: "r", At: future})
	if err != nil || ack.Status != AckStored {
		t.Fatalf("commit: %+v %v", ack, err)
	}
	if !st.turns[0].At.Equal(commit) {
		t.Fatalf("At = %v, want clamped to %v", st.turns[0].At, commit)
	}
	clk.Add(Retention + time.Hour)
	res, err := b.Prune(ctx, commit.Add(Retention+time.Nanosecond))
	if err != nil || res.Turns != 1 || len(st.turns) != 0 {
		t.Fatalf("prune = %+v %v, turns left %d", res, err, len(st.turns))
	}
}
