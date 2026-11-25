package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func init() { registerGrantHistoryModule(DefaultRegistry) }

func registerGrantHistoryModule(r *Registry) {
	if err := r.Register("grant_history", sessionControlModuleOrder+5, initGrantHistory); err != nil {
		panic(err)
	}
}

// initGrantHistory binds grant_history: the audit rows of one grant.
func initGrantHistory(_ context.Context, env *Env) error {
	if !catalogHas(ops.Default, ops.OpGrantHistory) {
		return nil
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	alog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	list := func(ctx context.Context) ([]api.GrantWorker, error) {
		rows, err := st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]api.GrantWorker, 0, len(rows))
		for _, w := range rows {
			out = append(out, api.GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Approved: w.ApprovedAt != 0})
		}
		return out, nil
	}
	if err := api.RegisterGrantHistory(handlers, st, alog, list, az); err != nil {
		return fmt.Errorf("grant_history: %w", err)
	}
	return nil
}

// auditedExpiredGrants makes the expiry sweep of the grants module write one
// audit row "grant.end" (cause expired) for each grant it deletes.
type auditedExpiredGrants struct{ st *store.Store }

func (a auditedExpiredGrants) DeleteExpiredSessionGrants(ctx context.Context, before int64) (int64, error) {
	return a.st.EndExpiredSessionGrants(ctx, before)
}
