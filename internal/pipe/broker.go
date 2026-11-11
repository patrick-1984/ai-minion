package pipe

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Config configures a Broker.
type Config struct {
	// Store is the persistence port. Required.
	Store Store
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
	// BudgetPerDay is the task budget (setting pipe_budget_per_day): the
	// messages one peer may send per 24 hours. Zero or less selects
	// DefaultBudgetPerDay.
	BudgetPerDay int
	// BudgetFunc, when set, is asked for the budget on every send and
	// replaces BudgetPerDay: a setting that changes while the broker runs
	// takes effect at once. A result of zero or less, or an error, selects
	// the budget of the last answer that worked (BudgetPerDay or the
	// default at first), so that a failing setting store never lifts the
	// limit.
	BudgetFunc func() (int, error)
}

// Broker is the one pipe broker (AD-23). It is safe for concurrent use.
type Broker struct {
	store   Store
	now     func() time.Time
	perDay  int
	budget  func() (int, error)
	limiter *sendLimiter

	sendMu sync.Mutex // serialises budget check, rate take and append
}

// NewBroker returns a broker over cfg.Store.
func NewBroker(cfg Config) (*Broker, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("pipe: Config.Store is required")
	}
	b := &Broker{store: cfg.Store, now: cfg.Now, perDay: cfg.BudgetPerDay, budget: cfg.BudgetFunc, limiter: newSendLimiter()}
	if b.now == nil {
		b.now = time.Now
	}
	if b.perDay <= 0 {
		b.perDay = DefaultBudgetPerDay
	}
	return b, nil
}

// BudgetPerDay returns the effective task budget.
func (b *Broker) BudgetPerDay() int {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.currentBudgetLocked()
}

// currentBudgetLocked asks BudgetFunc and remembers an answer that is usable.
func (b *Broker) currentBudgetLocked() int {
	if b.budget != nil {
		if n, err := b.budget(); err == nil && n > 0 {
			b.perDay = n
		}
	}
	return b.perDay
}

// Send validates and stores a message for peer. The checks run in this order
// and the first failure is returned as an *Error: invalid_argument,
// message_too_large, pipe_budget_exceeded, pipe_rate_limited. A rejected
// message is dropped: never queued, never delayed, never counted. Delivering
// the stored message to the session is the caller's job.
func (b *Broker) Send(ctx context.Context, peer Peer, text string) (Message, error) {
	if !peer.Valid() {
		return Message{}, newError(CodeInvalidArgument, "peer needs worker_id and session_name")
	}
	if text == "" {
		return Message{}, newError(CodeInvalidArgument, "message is empty")
	}
	if len(text) > MaxMessageBytes {
		return Message{}, newError(CodeMessageTooLarge,
			"message is %d bytes, the limit is %d", len(text), MaxMessageBytes)
	}

	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	now := b.now()
	if err := checkBudget(ctx, b.store, peer, b.currentBudgetLocked(), now); err != nil {
		return Message{}, err
	}
	if err := b.limiter.take(peer, now); err != nil {
		return Message{}, err
	}
	m, err := b.store.AppendMessage(ctx, Message{Peer: peer, Text: text, At: now})
	if err != nil {
		// A failed write must not use quota: give the send back.
		b.limiter.release(peer, now)
		return Message{}, fmt.Errorf("pipe: store message: %w", err)
	}
	return m, nil
}

// RefundDelivery returns the hourly delivery reservation that Send charged for
// m, after the receiving Worker definitively refused the message
// (pipe_origin_refused) and the caller retracted it. The attempt keeps its
// rate-limit token (attempt throttling stays); only the hour-window entry is
// given back. It reports whether a reservation was found. A send of unknown
// outcome is never refunded.
func (b *Broker) RefundDelivery(m Message) bool {
	return b.limiter.refund(m.Peer, m.At)
}

// CommitTurn stores a turn idempotently and answers with the turn.ack. A turn
// already stored under the same turn_id is not stored again and gets the same
// {status: "stored"} ack. An invalid turn is not stored and gets an
// {status: "rejected"} ack together with an invalid_argument *Error. Commits
// are not subject to the send limits or the budget: they are replies, not
// sends. The stored At is the client's timestamp, clamped to the broker's now. A store failure returns an error and no ack.
func (b *Broker) CommitTurn(ctx context.Context, t Turn) (Ack, error) {
	reject := func(reason string) (Ack, error) {
		return Ack{TurnID: t.TurnID, Status: AckRejected, Reason: reason},
			newError(CodeInvalidArgument, "%s", reason)
	}
	if !IsTurnID(t.TurnID) {
		return reject("turn_id is not a UUIDv7")
	}
	if !t.Peer.Valid() {
		return reject("peer needs worker_id and session_name")
	}
	// The broker clock decides the age of a turn: a missing or future
	// client timestamp is replaced by now, so a peer cannot keep a turn
	// beyond the retention (AD-23).
	if now := b.now(); t.At.IsZero() || t.At.After(now) {
		t.At = now
	}
	if _, err := b.store.PutTurn(ctx, t); err != nil {
		return Ack{}, fmt.Errorf("pipe: store turn: %w", err)
	}
	return Ack{TurnID: t.TurnID, Status: AckStored}, nil
}

// Messages returns the stored messages of peer after afterID, oldest first.
func (b *Broker) Messages(ctx context.Context, peer Peer, afterID int64, limit int) ([]Message, error) {
	return b.store.Messages(ctx, peer, afterID, limit)
}

// Turns returns the stored turns of peer after afterTurnID in commit order. It
// only reads: the broker never forwards or replays turns by itself.
func (b *Broker) Turns(ctx context.Context, peer Peer, afterTurnID string, limit int) ([]Turn, error) {
	if afterTurnID != "" && !IsTurnID(afterTurnID) {
		return nil, newError(CodeInvalidArgument, "after_turn_id is not a UUIDv7")
	}
	return b.store.Turns(ctx, peer, afterTurnID, limit)
}

// PruneResult counts what Prune deleted.
type PruneResult struct {
	Messages int
	Turns    int
}

// Prune deletes messages and turns older than the 7-day retention, measured
// from now.
func (b *Broker) Prune(ctx context.Context, now time.Time) (PruneResult, error) {
	m, t, err := b.store.DeleteBefore(ctx, now.Add(-Retention))
	if err != nil {
		return PruneResult{}, fmt.Errorf("pipe: prune: %w", err)
	}
	return PruneResult{Messages: m, Turns: t}, nil
}
