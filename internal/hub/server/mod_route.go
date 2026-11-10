package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// routeModuleOrder places the routing module after the workers module (30) and
// the api module (40), whose service and handler registry it needs.
const routeModuleOrder = 50

func init() { registerRouteModule(DefaultRegistry) }

// registerRouteModule registers the routing module in r (Story 5.1). It
// provides no service: it builds the [workers.Router] and registers the
// handlers of workers_list and of the core Worker ops on the api handler
// registry, so that a running Hub routes them to the connected Workers
// instead of answering unsupported_op. It also gives the dispatcher the router
// as target resolver, so that a request addresses a session as
// "worker-name:session-name" and is resolved for the authenticated caller.
//
// The three pipe ops are not plain-routed here: the Hub is the pipe broker
// (AD-23), and the pipe module (Story 7.4, mod_pipe.go) registers them with
// the broker, over the router this module provides. Without the pipe module a
// Hub answers them with unsupported_op; it never routes pipe_send past the
// broker, which would skip the limits, the budget and the history.
func registerRouteModule(r *Registry) {
	if err := r.Register("route", routeModuleOrder, initRoute, DeclaresService[*workers.Router]()); err != nil {
		panic(err)
	}
}

func initRoute(ctx context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	rt, err := workers.NewRouter(workers.RouterConfig{
		Backend:   workers.RegistryBackend(svc.Registry()),
		Directory: workers.StoreDirectory{Store: st},
		Authz:     az,
		OwnerName: func(ctx context.Context, ownerID string) string {
			u, err := st.UserByID(ctx, ownerID)
			if err != nil {
				return ""
			}
			return u.Username
		},
		Logger: cfg.logger(),
	})
	if err != nil {
		return fmt.Errorf("route: %w", err)
	}
	if err := api.RegisterWorkersList(handlers, rt); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	for _, name := range ops.CoreWorkerOpNames {
		switch name {
		case ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait:
			continue // served by the pipe module
		}
		if err := api.RegisterWorkerOp(handlers, name, rt); err != nil {
			return fmt.Errorf("route: %w", err)
		}
	}
	// The same router resolves the human form of a target (pi-1:minion-3)
	// for the authenticated caller, on every Worker op.
	if err := api.RegisterTargetResolution(handlers, rt); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	return Provide(env, rt)
}
