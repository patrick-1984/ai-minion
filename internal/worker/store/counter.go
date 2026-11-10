package store

import (
	"context"
	"fmt"
)

// SessionPrefix is the name prefix of sessions the Worker creates (R74).
const SessionPrefix = "minion-"

// SessionName returns the tmux session name for counter value n, "minion-N".
func SessionName(n int64) string { return fmt.Sprintf("%s%d", SessionPrefix, n) }

// NextSessionNumber increments the persisted session counter and returns the
// new value: 1 on a new database, then 2, 3, and so on. The increment is one
// atomic statement, so concurrent callers (also in other processes) get
// distinct numbers, and the value survives a reopen. A number is never handed
// out twice, whether or not its session still exists (AD-13, R74).
func (s *Store) NextSessionNumber() (int64, error) {
	return s.NextSessionNumberContext(context.Background())
}

// NextSessionNumberContext is [Store.NextSessionNumber] with a context.
func (s *Store) NextSessionNumberContext(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE session_counter SET last = last + 1 WHERE id = 1 RETURNING last`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: next session number: %w", err)
	}
	return n, nil
}
