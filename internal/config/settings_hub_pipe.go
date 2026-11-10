package config

import (
	"fmt"
	"strconv"
)

// KeyPipeBudgetPerDay names the setting of the pipe task budget (AD-23, Story
// 7.4): the messages one peer {worker_id, session_name} may be sent per 24
// hours. Its environment name is MINION_PIPE_BUDGET_PER_DAY. The Hub resolves
// it for every send, so a value stored in the panel takes effect at once.
const KeyPipeBudgetPerDay = "pipe_budget_per_day"

// Default and bounds of pipe_budget_per_day. The default is the recorded
// default decision of Story 7.1 (200 messages per peer per 24 hours).
const (
	DefaultPipeBudgetPerDay = 200
	MinPipeBudgetPerDay     = 1
	MaxPipeBudgetPerDay     = 100000
)

func init() { declareHubPipe(Default) }

// declareHubPipe declares the settings of the Hub pipe broker.
func declareHubPipe(r *Registry) {
	r.Declare(Setting{Key: KeyPipeBudgetPerDay, Scope: ScopeHub, Default: strconv.Itoa(DefaultPipeBudgetPerDay),
		Help: "Task budget of the pipe: the messages one session may be sent per 24 hours (1 to 100000)"})
}

// ParsePipeBudgetPerDay parses a value of pipe_budget_per_day and applies the
// bounds.
func ParsePipeBudgetPerDay(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a whole number, got %q", KeyPipeBudgetPerDay, v)
	}
	if n < MinPipeBudgetPerDay || n > MaxPipeBudgetPerDay {
		return 0, fmt.Errorf("config: %s must be between %d and %d, got %d", KeyPipeBudgetPerDay, MinPipeBudgetPerDay, MaxPipeBudgetPerDay, n)
	}
	return n, nil
}

// ResolvePipeBudgetPerDay resolves pipe_budget_per_day through r, a Hub
// resolver, and parses it.
func ResolvePipeBudgetPerDay(r *Resolver) (int, error) {
	res, err := r.Resolve(KeyPipeBudgetPerDay)
	if err != nil {
		return 0, err
	}
	n, err := ParsePipeBudgetPerDay(res.Value)
	if err != nil {
		return 0, fmt.Errorf("%w (from %s)", err, res.Source)
	}
	return n, nil
}
