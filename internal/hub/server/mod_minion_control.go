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
// rows in one read-only transaction (one commit, never the write lock) and
// the live state: who is connected, the sessions the Hub lists, which
// session-bound masters hold a lease now, and which terminal incarnations
// (Worker, session, incarnation) hold a live master lease, so a verified
// grant bound to one of them is in effect exactly when authorization would
// accept its lease.
func accessFacts(ctx context.Context, env *Env, st *store.Store, svc *workers.Service) (api.AccessFacts, error) {
	snap, err := st.AccessSnapshot(ctx)
	if err != nil {
		return api.AccessFacts{}, err
	}
	live := api.AccessLive{Online: map[string]bool{}, AgentOnly: map[string]bool{}, Sessions: map[string][]string{},
		Attached: map[[2]string]bool{}, Leases: map[[3]string]bool{}}
	for _, o := range svc.Registry().List() {
		live.Online[o.WorkerID] = true
		live.AgentOnly[o.WorkerID] = o.HasFeature(proto.FeatureSendTextAgentOnly)
	}
	if hub, err := Require[*workers.EventHub](env); err == nil {
		for _, ws := range hub.All() {
			for _, s := range ws.Sessions {
				live.Sessions[ws.WorkerID] = append(live.Sessions[ws.WorkerID], s.Name)
			}
		}
	}
	if master, err := Require[*workers.MasterControl](env); err == nil {
		view, err := master.MasterView(ctx)
		if err != nil {
			return api.AccessFacts{}, err
		}
		for id, v := range view {
			for _, b := range v.Bound {
				if b.Master && b.Attachment == "active" {
					live.Attached[[2]string{id, b.SessionName}] = true
				}
			}
		}
		// The leases themselves, never the candidates: an eligible candidate
		// has no lease until the next report or refresh issues one.
		for _, l := range master.LiveLeases() {
			live.Leases[[3]string{l.WorkerID, l.SessionName, l.IncarnationID}] = true
		}
	}
	return api.AccessFactsFrom(snap, live), nil
}

// canBeMaster tells whether a Worker can run master sessions: a full Worker
// names its build in the hello (0.9.0 or later) or announces pipe.causal
// (0.5.0 or later); a thin Worker does neither. An offline Worker is not
// known yet and may be nominated. It only refuses a nomination in the
// editor; it never grants anything.
func canBeMaster(o workers.Online, online bool) bool {
	return !online || o.Facts.Build != "" || o.HasFeature(proto.FeaturePipeCausal)
}
