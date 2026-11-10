package pipe

import (
	"context"
	"fmt"
	"time"
)

// Budget defaults (recorded decision, EPICS "Pipe budget default").
const (
	// DefaultBudgetPerDay is the default task budget: messages per peer per
	// BudgetWindow. The setting is pipe_budget_per_day.
	DefaultBudgetPerDay = 200
	// BudgetWindow is the window of the task budget.
	BudgetWindow = 24 * time.Hour
)

// checkBudget returns pipe_budget_exceeded when peer has already stored perDay
// messages in the last BudgetWindow. It has no side effects: the budget is the
// count of stored messages, so a rejected message never uses budget.
func checkBudget(ctx context.Context, s Store, peer Peer, perDay int, now time.Time) error {
	n, err := s.CountMessagesSince(ctx, peer, now.Add(-BudgetWindow))
	if err != nil {
		return fmt.Errorf("pipe: count messages: %w", err)
	}
	if n >= perDay {
		return newError(CodePipeBudgetExceeded,
			"task budget of %d messages per 24 hours used up", perDay)
	}
	return nil
}
