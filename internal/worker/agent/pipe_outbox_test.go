package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func TestStoreOutboxKeepsTurnsAcrossARestartOfTheDatabase(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := NewStoreOutbox(st, 2)
	a := PendingTurn{TurnID: ops.NewUUIDv7(), MessageID: 7, Session: "work", Text: "one", At: time.UnixMilli(1_700_000_000_000)}
	b := PendingTurn{TurnID: ops.NewUUIDv7(), MessageID: 7, Session: "work", Text: "two", At: time.UnixMilli(1_700_000_001_000)}
	c := PendingTurn{TurnID: ops.NewUUIDv7(), MessageID: 7, Session: "work", Text: "three"}
	for _, x := range []PendingTurn{a, b} {
		if err := o.Add(x); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Add(a); err != nil {
		t.Fatalf("adding a known turn changes nothing: %v", err)
	}
	if err := o.Add(c); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("a full outbox must return ErrOutboxFull, got %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	o = NewStoreOutbox(st, 2)
	got, err := o.Pending()
	if err != nil || len(got) != 2 || got[0].TurnID != a.TurnID || got[0].Text != "one" || !got[0].At.Equal(a.At) || got[1].TurnID != b.TurnID {
		t.Fatalf("pending after the restart = %+v, %v", got, err)
	}
	if err := o.Remove(a.TurnID); err != nil {
		t.Fatal(err)
	}
	if got, _ := o.Pending(); len(got) != 1 || got[0].TurnID != b.TurnID {
		t.Fatalf("pending = %+v", got)
	}
}

// The emitter over the durable outbox: a restart between the reply and the
// Hub's turn.ack loses nothing, and the same turn_id is sent again.
func TestTurnEmitterOverTheStoreOutboxResendsAfterARestart(t *testing.T) {
	dir := t.TempDir()
	open := func() (*store.Store, *TurnEmitter) {
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
			t.Fatal(err)
		}
		em, err := NewTurnEmitter(TurnConfig{Shares: NewPolicy(st), Outbox: NewStoreOutbox(st, 0)})
		if err != nil {
			t.Fatal(err)
		}
		return st, em
	}
	st, em := open()
	id, err := em.Emit(PendingTurn{MessageID: 7, Session: "work", Text: "the reply"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil { // the Worker stops before any ack
		t.Fatal(err)
	}
	st, em = open()
	defer func() { _ = st.Close() }()
	pending, err := em.out.Pending()
	if err != nil || len(pending) != 1 || pending[0].TurnID != id || pending[0].Text != "the reply" {
		t.Fatalf("pending after the restart = %+v, %v", pending, err)
	}
}
