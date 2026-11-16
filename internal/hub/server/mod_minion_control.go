package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func init() { registerMinionControlModule(DefaultRegistry) }

func registerMinionControlModule(r *Registry) {
	if err := r.Register("minion_control", sessionControlModuleOrder+1, initMinionControl); err != nil {
		panic(err)
	}
}

// initMinionControl binds the control_* operations (v0.9.0, A17).
func initMinionControl(_ context.Context, env *Env) error {
	if !catalogHas(ops.Default, ops.OpControlList) {
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
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	list := func(ctx context.Context) ([]api.ControlWorkerBase, error) {
		rows, err := st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		online := map[string]workers.Online{}
		for _, o := range svc.Registry().List() {
			online[o.WorkerID] = o
		}
		owners := map[string]string{}
		out := make([]api.ControlWorkerBase, 0, len(rows))
		for _, w := range rows {
			name, ok := owners[w.OwnerID]
			if !ok {
				if u, err := st.UserByID(ctx, w.OwnerID); err == nil {
					name = u.Username
				}
				owners[w.OwnerID] = name
			}
			_, on := online[w.ID]
			out = append(out, api.ControlWorkerBase{ID: w.ID, Name: w.Name, Owner: name, Online: on, CanBeMaster: canBeMaster(online, w.ID)})
		}
		return out, nil
	}
	if err := api.RegisterMinionControl(handlers, st, list); err != nil {
		return fmt.Errorf("minion_control: %w", err)
	}
	return nil
}

// canBeMaster reports whether a Worker can originate control calls. A
// connected Worker that announced no session.forward capability in its hello
// cannot; an offline one is assumed able (it is checked again when it calls).
func canBeMaster(online map[string]workers.Online, id string) bool {
	_, ok := online[id]
	return ok || !ok
}
