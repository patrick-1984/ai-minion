package agent

// Story 7.3: the durable turn outbox (AD-23, AD-16).
//
// The turn emitter (Story 7.4) keeps a turn until the Hub answers turn.ack. Its
// default outbox is in memory, so a restart of the Worker between the reply
// and the ack would lose the reply with no trace. [StoreOutbox] keeps the
// turns in the Worker's database (table pipe_outbox of migration 0004) instead:
// a turn is removed only with the ack, and the emitter sends what is pending
// again at the first link after a restart, with the same turn_id, so the Hub
// commits it once. "minion worker reset" empties the outbox with the
// enrollment it belongs to.

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// outboxTimeout bounds one database call of the outbox.
const outboxTimeout = 10 * time.Second

// OutboxStore is the part of the Worker database the outbox uses.
// *store.Store implements it.
type OutboxStore interface {
	OutboxAdd(ctx context.Context, t store.OutboxTurn, limit int) error
	OutboxPending(ctx context.Context) ([]store.OutboxTurn, error)
	OutboxRemove(ctx context.Context, turnID string) error
}

var _ OutboxStore = (*store.Store)(nil)

// StoreOutbox is a [TurnOutbox] over the Worker database.
type StoreOutbox struct {
	st    OutboxStore
	limit int
}

var _ TurnOutbox = (*StoreOutbox)(nil)

// NewStoreOutbox returns an outbox that holds at most limit turns (zero or
// less selects [DefaultOutboxLimit]).
func NewStoreOutbox(st OutboxStore, limit int) *StoreOutbox {
	if limit <= 0 {
		limit = DefaultOutboxLimit
	}
	return &StoreOutbox{st: st, limit: limit}
}

// Add implements [TurnOutbox].
func (o *StoreOutbox) Add(t PendingTurn) error {
	ctx, cancel := context.WithTimeout(context.Background(), outboxTimeout)
	defer cancel()
	err := o.st.OutboxAdd(ctx, store.OutboxTurn{TurnID: t.TurnID, Session: t.Session, MessageID: t.MessageID, Text: t.Text, At: t.At}, o.limit)
	if errors.Is(err, store.ErrOutboxFull) {
		return ErrOutboxFull
	}
	return err
}

// Pending implements [TurnOutbox].
func (o *StoreOutbox) Pending() ([]PendingTurn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), outboxTimeout)
	defer cancel()
	rows, err := o.st.OutboxPending(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PendingTurn, len(rows))
	for i, r := range rows {
		out[i] = PendingTurn{TurnID: r.TurnID, Session: r.Session, MessageID: r.MessageID, Text: r.Text, At: r.At}
	}
	return out, nil
}

// Remove implements [TurnOutbox].
func (o *StoreOutbox) Remove(turnID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), outboxTimeout)
	defer cancel()
	return o.st.OutboxRemove(ctx, turnID)
}
