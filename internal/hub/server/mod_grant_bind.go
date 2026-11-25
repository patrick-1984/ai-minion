package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func init() { registerGrantBindModule(DefaultRegistry) }

func registerGrantBindModule(r *Registry) {
	if err := r.Register("grant_bind", eventsModuleOrder+2, initGrantBind); err != nil {
		panic(err)
	}
}

// initGrantBind binds grant_bind: a session grant bound to the verified
// terminal of its source session (the master lane knows the live terminals).
func initGrantBind(_ context.Context, env *Env) error {
	if !catalogHas(ops.Default, ops.OpGrantBind) {
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
	master, err := Require[*workers.MasterControl](env)
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
		owners := map[string]string{}
		out := make([]api.GrantWorker, 0, len(rows))
		for _, w := range rows {
			name, ok := owners[w.OwnerID]
			if !ok {
				if u, err := st.UserByID(ctx, w.OwnerID); err == nil {
					name = u.Username
				}
				owners[w.OwnerID] = name
			}
			out = append(out, api.GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Owner: name, Approved: w.ApprovedAt != 0})
		}
		return out, nil
	}
	if err := api.RegisterGrantBind(handlers, st, master, list, az, alog); err != nil {
		return fmt.Errorf("grant_bind: %w", err)
	}
	return nil
}
