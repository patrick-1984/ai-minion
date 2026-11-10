package pipe

import (
	"context"
	"time"
)

// Store is the persistence port of the broker. The Hub implements it over
// SQLite tables pipe_messages and pipe_budgets (Story 7.4) and the Worker over
// its worker.db for Solo mode (Story 7.3). All methods must be safe for
// concurrent use. The store holds no policy: limits, budget and retention are
// decided by the broker.
type Store interface {
	// AppendMessage stores m and returns it with its ID assigned. IDs grow
	// strictly with insertion order.
	AppendMessage(ctx context.Context, m Message) (Message, error)

	// Messages returns the messages of peer with ID greater than afterID,
	// oldest first, at most limit (limit <= 0 means no limit).
	Messages(ctx context.Context, peer Peer, afterID int64, limit int) ([]Message, error)

	// CountMessagesSince returns the number of messages of peer stored at or
	// after since. It feeds the task budget.
	CountMessagesSince(ctx context.Context, peer Peer, since time.Time) (int, error)

	// PutTurn stores t unless a turn with the same TurnID exists. It reports
	// whether t was newly stored. A repeated TurnID is not an error and does
	// not change the stored turn.
	PutTurn(ctx context.Context, t Turn) (inserted bool, err error)

	// Turns returns the turns of peer committed after the turn afterTurnID
	// (empty means from the oldest), in commit order, at most limit
	// (limit <= 0 means no limit). A non-empty afterTurnID that is unknown for
	// peer yields an *Error with CodeNotFound.
	Turns(ctx context.Context, peer Peer, afterTurnID string, limit int) ([]Turn, error)

	// DeleteBefore deletes the messages and turns whose At is before cutoff
	// and returns how many of each.
	DeleteBefore(ctx context.Context, cutoff time.Time) (messages, turns int, err error)
}
