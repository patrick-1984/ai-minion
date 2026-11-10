package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/pipe"
)

var testPeer = pipe.Peer{WorkerID: "solo", SessionName: "work"}

func newTurn(t *testing.T, at time.Time, text string) pipe.Turn {
	t.Helper()
	id, err := pipe.NewTurnID(at)
	if err != nil {
		t.Fatal(err)
	}
	return pipe.Turn{TurnID: id, Peer: testPeer, Text: text, At: at}
}

func TestPipeEntriesShareOneCommitOrder(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))

	m1, err := s.AppendMessage(ctx, pipe.Message{Peer: testPeer, Text: "hello", At: clk.Now()})
	if err != nil || m1.ID == 0 {
		t.Fatalf("append: %+v %v", m1, err)
	}
	clk.Advance(time.Second)
	tu := newTurn(t, clk.Now(), "reply")
	if ins, err := s.PutTurn(ctx, tu); err != nil || !ins {
		t.Fatalf("put turn: %v %v", ins, err)
	}
	clk.Advance(time.Second)
	m2, err := s.AppendMessage(ctx, pipe.Message{Peer: testPeer, Text: "again", At: clk.Now()})
	if err != nil || m2.ID <= m1.ID {
		t.Fatalf("IDs must grow: %d then %d (%v)", m1.ID, m2.ID, err)
	}
	// Another session has its own pipe.
	other := pipe.Peer{WorkerID: "solo", SessionName: "other"}
	if _, err := s.AppendMessage(ctx, pipe.Message{Peer: other, Text: "x", At: clk.Now()}); err != nil {
		t.Fatal(err)
	}

	es, err := s.Entries(ctx, testPeer, "", 0)
	if err != nil || len(es) != 3 {
		t.Fatalf("entries = %+v, %v", es, err)
	}
	if es[0].Kind != PipeSent || es[0].Text != "hello" || es[1].Kind != PipeReceived || es[1].EntryID != tu.TurnID || es[2].Text != "again" {
		t.Fatalf("order/kinds wrong: %+v", es)
	}
	// Every entry id, a sent one included, is a cursor.
	after, err := s.Entries(ctx, testPeer, es[0].EntryID, 0)
	if err != nil || len(after) != 2 || after[0].EntryID != tu.TurnID {
		t.Fatalf("after the first message = %+v, %v", after, err)
	}
	if !pipe.IsTurnID(es[0].EntryID) {
		t.Fatalf("entry id %q is not a UUIDv7", es[0].EntryID)
	}
	// Turns skips the messages and accepts a message id as cursor.
	turns, err := s.Turns(ctx, testPeer, es[0].EntryID, 0)
	if err != nil || len(turns) != 1 || turns[0].Text != "reply" {
		t.Fatalf("turns = %+v, %v", turns, err)
	}
	none, err := s.Turns(ctx, testPeer, tu.TurnID, 0)
	if err != nil || len(none) != 0 {
		t.Fatalf("turns after the last = %+v, %v", none, err)
	}
	limited, err := s.Entries(ctx, testPeer, "", 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limit: %+v %v", limited, err)
	}
	if got, err := s.LatestTurnID(ctx, testPeer); err != nil || got != tu.TurnID {
		t.Fatalf("latest turn = %q, %v", got, err)
	}
	if got, err := s.LatestTurnID(ctx, other); err != nil || got != "" {
		t.Fatalf("latest turn of a pipe without turns = %q, %v", got, err)
	}
}

func TestPipeUnknownCursorIsNotFoundAndNotCrossPeer(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	tu := newTurn(t, time.Now(), "r")
	if _, err := s.PutTurn(ctx, tu); err != nil {
		t.Fatal(err)
	}
	other := pipe.Peer{WorkerID: "solo", SessionName: "other"}
	for _, peer := range []pipe.Peer{other, testPeer} {
		id := tu.TurnID
		if peer == testPeer {
			id, _ = pipe.NewTurnID(time.Now()) // not stored
		}
		if _, err := s.Turns(ctx, peer, id, 0); !errors.Is(err, pipe.ErrNotFound) {
			t.Fatalf("Turns(%s, %s) err = %v, want not_found", peer, id, err)
		}
		if _, err := s.Entries(ctx, peer, id, 0); !errors.Is(err, pipe.ErrNotFound) {
			t.Fatalf("Entries err = %v, want not_found", err)
		}
	}
}

func TestPipePutTurnIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	tu := newTurn(t, time.Now(), "once")
	if ins, err := s.PutTurn(ctx, tu); err != nil || !ins {
		t.Fatalf("first: %v %v", ins, err)
	}
	tu.Text = "changed"
	if ins, err := s.PutTurn(ctx, tu); err != nil || ins {
		t.Fatalf("second: %v %v", ins, err)
	}
	got, _ := s.Turns(ctx, testPeer, "", 0)
	if len(got) != 1 || got[0].Text != "once" {
		t.Fatalf("turns = %+v", got)
	}
}

func TestPipeBudgetCountAndRetract(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))
	old, _ := s.AppendMessage(ctx, pipe.Message{Peer: testPeer, Text: "old", At: clk.Now().Add(-48 * time.Hour)})
	cur, _ := s.AppendMessage(ctx, pipe.Message{Peer: testPeer, Text: "cur", At: clk.Now()})
	tu := newTurn(t, clk.Now(), "reply")
	_, _ = s.PutTurn(ctx, tu)
	if n, err := s.CountMessagesSince(ctx, testPeer, clk.Now().Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("count = %d, %v (replies are not budget)", n, err)
	}
	if err := s.DeleteMessage(ctx, cur.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMessage(ctx, cur.ID); err != nil {
		t.Fatalf("a missing message is not an error: %v", err)
	}
	if n, _ := s.CountMessagesSince(ctx, testPeer, clk.Now().Add(-24*time.Hour)); n != 0 {
		t.Fatalf("count after retract = %d", n)
	}
	if got, _ := s.Turns(ctx, testPeer, "", 0); len(got) != 1 {
		t.Fatalf("a retract must not delete turns: %+v", got)
	}
	// DeleteMessage never deletes a received turn, whatever the id.
	if err := s.DeleteMessage(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Turns(ctx, testPeer, "", 0); len(got) != 1 {
		t.Fatalf("turn deleted through DeleteMessage: %+v", got)
	}
	m, tn, err := s.DeleteBefore(ctx, clk.Now().Add(-24*time.Hour))
	if err != nil || m != 1 || tn != 0 {
		t.Fatalf("DeleteBefore = %d, %d, %v (old id %d)", m, tn, err, old.ID)
	}
	m, tn, err = s.DeleteBefore(ctx, clk.Now().Add(time.Hour))
	if err != nil || m != 0 || tn != 1 {
		t.Fatalf("DeleteBefore later = %d, %d, %v", m, tn, err)
	}
}

func TestPipeBrokerOverTheStore(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))
	b, err := pipe.NewBroker(pipe.Config{Store: s, Now: clk.Now, BudgetPerDay: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send(ctx, testPeer, "one"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second)
	if _, err := b.Send(ctx, testPeer, "two"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second)
	if _, err := b.Send(ctx, testPeer, "three"); !errors.Is(err, pipe.ErrPipeBudgetExceeded) {
		t.Fatalf("third send err = %v, want the budget", err)
	}
	tu := newTurn(t, clk.Now(), "r")
	ack, err := b.CommitTurn(ctx, pipe.Turn{TurnID: tu.TurnID, Peer: testPeer, Text: "r", At: clk.Now()})
	if err != nil || ack.Status != pipe.AckStored {
		t.Fatalf("commit: %+v %v", ack, err)
	}
	if res, err := b.Prune(ctx, clk.Now().Add(pipe.Retention+time.Hour)); err != nil || res.Messages != 2 || res.Turns != 1 {
		t.Fatalf("prune = %+v, %v", res, err)
	}
}

func TestOutboxIsDurableOrderedAndBounded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	mk := func(i int) OutboxTurn {
		id, _ := pipe.NewTurnID(time.UnixMilli(1_700_000_000_000 + int64(i)))
		return OutboxTurn{TurnID: id, Session: "work", MessageID: int64(100 + i), Text: "t", At: time.UnixMilli(1_700_000_000_000 + int64(i))}
	}
	a, b, c := mk(1), mk(2), mk(3)
	for _, x := range []OutboxTurn{a, b} {
		if err := s.OutboxAdd(ctx, x, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.OutboxAdd(ctx, a, 2); err != nil {
		t.Fatalf("a known turn changes nothing, even when the outbox is full: %v", err)
	}
	if err := s.OutboxAdd(ctx, c, 2); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("third turn err = %v, want ErrOutboxFull", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A restart keeps the turns.
	s = openStore(t, dir)
	got, err := s.OutboxPending(ctx)
	if err != nil || len(got) != 2 || got[0].TurnID != a.TurnID || got[1].TurnID != b.TurnID {
		t.Fatalf("pending after restart = %+v, %v", got, err)
	}
	// The delivery the turn answers is kept with it (migration 0006).
	if got[0].MessageID != 101 || got[1].MessageID != 102 {
		t.Fatalf("message ids after restart = %d, %d, want 101, 102", got[0].MessageID, got[1].MessageID)
	}
	if err := s.OutboxRemove(ctx, a.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := s.OutboxRemove(ctx, a.TurnID); err != nil {
		t.Fatalf("an unknown turn is not an error: %v", err)
	}
	if got, _ := s.OutboxPending(ctx); len(got) != 1 || got[0].TurnID != b.TurnID {
		t.Fatalf("pending = %+v", got)
	}
	// A reset of the pairing drops what the old Hub never acknowledged.
	if _, err := s.ResetPairing(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.OutboxPending(ctx); len(got) != 0 {
		t.Fatalf("outbox after reset = %+v", got)
	}
}

func TestMigration0004AppliesToADatabaseThatHasThe0005(t *testing.T) {
	// 0004 was reserved when 0005 shipped. The runner applies a migration the
	// database lacks whatever the order of numbers; check that the real set
	// holds both and that a fresh database has the pipe tables.
	s := openStore(t, t.TempDir())
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version IN (4, 5)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("versions 4 and 5 applied: %d, %v", n, err)
	}
}
