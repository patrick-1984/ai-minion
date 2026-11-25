package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
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
		var bound map[string]workers.MasterWorkerView
		if master, err := Require[*workers.MasterControl](env); err == nil {
			bound, err = master.MasterView(ctx)
			if err != nil {
				return nil, err
			}
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
			o, on := online[w.ID]
			view := bound[w.ID]
			out = append(out, api.ControlWorkerBase{ID: w.ID, Name: w.Name, Owner: name, OwnerID: w.OwnerID, Online: on,
				CanBeMaster: canBeMaster(o, on), MasterSessionV1: o.MasterSessionV1, MasterCandidates: view.Candidates, BoundMasters: view.Bound})
		}
		return out, nil
	}
	check := func(ctx context.Context, workerID, session string) ([]ops.ControlChain, error) {
		master, err := Require[*workers.MasterControl](env)
		if err != nil {
			return nil, nil
		}
		return master.Controllers(ctx, workerID, session)
	}
	if err := api.RegisterMinionControl(handlers, st, list, check); err != nil {
		return fmt.Errorf("minion_control: %w", err)
	}
	accessAdmin := func(ctx context.Context, userID string) (bool, error) {
		u, err := st.UserByID(ctx, userID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil && u.Active && u.Role == "admin", err
	}
	loadAccess := func(ctx context.Context) (api.AccessFacts, error) { return accessFacts(ctx, env, st, svc) }
	if catalogHas(ops.Default, ops.OpAccessMatrix) {
		if err := api.RegisterAccessMatrix(handlers, loadAccess, accessAdmin); err != nil {
			return fmt.Errorf("access_matrix: %w", err)
		}
	}
	if catalogHas(ops.Default, ops.OpAccessExplain) {
		if err := api.RegisterAccessExplain(handlers, loadAccess, accessAdmin); err != nil {
			return fmt.Errorf("access_explain: %w", err)
		}
	}
	return nil
}

// accessFacts reads what the access matrix is computed from: the durable
// rows (plain reads, never a write transaction) and the live session lists.
func accessFacts(ctx context.Context, env *Env, st *store.Store, svc *workers.Service) (api.AccessFacts, error) {
	var f api.AccessFacts
	rows, err := st.ListWorkers(ctx)
	if err != nil {
		return f, err
	}
	if f.Masters, err = st.ListMasterSessions(ctx); err != nil {
		return f, err
	}
	if f.Accepts, err = st.ListAccepts(ctx); err != nil {
		return f, err
	}
	if f.Control, err = st.ListWorkerControl(ctx); err != nil {
		return f, err
	}
	if f.Shares, err = st.LoadWorkerShares(ctx); err != nil {
		return f, err
	}
	if f.Bound, err = st.ListBoundMasters(ctx, ""); err != nil {
		return f, err
	}
	if f.Grants, err = st.ListSessionGrants(ctx, store.SessionGrantFilter{}); err != nil {
		return f, err
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return f, err
	}
	f.Admins = map[string]bool{}
	for _, u := range users {
		if u.Active && u.Role == "admin" {
			f.Admins[u.ID] = true
		}
	}
	online := map[string]bool{}
	for _, o := range svc.Registry().List() {
		online[o.WorkerID] = true
	}
	live := map[string][]string{}
	if hub, err := Require[*workers.EventHub](env); err == nil {
		for _, ws := range hub.All() {
			for _, s := range ws.Sessions {
				live[ws.WorkerID] = append(live[ws.WorkerID], s.Name)
			}
		}
	}
	type owner struct {
		name  string
		admin bool
	}
	owners := map[string]owner{}
	for _, w := range rows {
		o, ok := owners[w.OwnerID]
		if !ok {
			if u, err := st.UserByID(ctx, w.OwnerID); err == nil {
				o = owner{u.Username, u.Active && u.Role == "admin"}
			}
			owners[w.OwnerID] = o
		}
		f.Workers = append(f.Workers, api.AccessWorker{ID: w.ID, Name: w.Name, Owner: o.name, OwnerID: w.OwnerID, OwnerAdmin: o.admin,
			Pending: w.Pending, Online: online[w.ID], Sessions: live[w.ID]})
	}
	return f, nil
}

// canBeMaster tells whether a Worker can run master sessions: a full Worker
// names its build in the hello (0.9.0 or later) or announces pipe.causal
// (0.5.0 or later); a thin Worker does neither. An offline Worker is not
// known yet and may be nominated. It only refuses a nomination in the
// editor; it never grants anything.
func canBeMaster(o workers.Online, online bool) bool {
	return !online || o.Facts.Build != "" || o.HasFeature(proto.FeaturePipeCausal)
}
