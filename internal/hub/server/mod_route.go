package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// routeModuleOrder places the routing module after the workers module (30) and
// the api module (40), whose service and handler registry it needs.
const routeModuleOrder = 50

func init() {
	// Live: session_create resolves hidden_sessions on every call.
	DeclareLiveSetting(config.KeyHiddenSessions)
	registerRouteModule(DefaultRegistry)
}

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
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	// hidden_sessions is resolved on every call (live); a value that does not
	// resolve counts as disabled (fail closed).
	resolver := settings.Resolver()
	hubPolicy := func() string {
		r, err := resolver.Resolve(config.KeyHiddenSessions)
		if err != nil || r.Value == "" {
			return config.HiddenSessionsDisabled
		}
		return r.Value
	}
	rt, err := workers.NewRouter(workers.RouterConfig{
		Telemetry: svc.Telemetry,
		Monitor:   complianceMonitor(env),
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
		Logger:         cfg.logger(),
		UserManaged:    userManaged(st),
		ContentManaged: contentManaged(st),
		HiddenSessions: hiddenSessionsPolicy(hubPolicy, st),
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
	// Central Worker update (v0.9.0): admin-only ops routed like the others
	// (the dispatcher decides the action manage on the named Worker). Remote
	// debug (v0.10.0): the owner or an admin (operate or manage), audited; the
	// Worker itself refuses unless it allows it.
	for _, name := range []string{ops.OpWorkerUpdate, ops.OpWorkerUpdateStatus, ops.OpWorkerDebug, ops.OpWorkerLogs} {
		if err := api.RegisterWorkerOp(handlers, name, rt); err != nil {
			return fmt.Errorf("route: %w", err)
		}
	}
	// The caller's own preferences (account_prefs, account_prefs_set), next to
	// the hidden-sessions policy they feed.
	allows := func(hub string, user bool) bool {
		return workers.HiddenSessionsPolicy{Hub: hub, UserAllows: user}.Allows()
	}
	if err := api.RegisterAccountPrefs(handlers, st, az, hubPolicy, allows); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	// The same router resolves the human form of a target (pi-1:minion-3)
	// for the authenticated caller, on every Worker op.
	if err := api.RegisterTargetResolution(handlers, rt); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	return Provide(env, rt)
}

// userManaged reads the user-managed switch of a Worker (Minion control).
// contentManaged is userManaged for recording session content: a Worker the
// Hub no longer has (it is being removed) is not user-managed, whatever the
// default of a missing control row says.
func contentManaged(st *store.Store) func(ctx context.Context, workerID string) (bool, error) {
	return st.WorkerContentManaged
}

func userManaged(st *store.Store) func(ctx context.Context, workerID string) (bool, error) {
	return func(ctx context.Context, workerID string) (bool, error) {
		c, err := st.WorkerControl(ctx, workerID)
		return c.UserManaged, err
	}
}

// hiddenSessionsPolicy returns the hidden-sessions policy of a caller: the
// Hub setting hidden_sessions, resolved on every call (live), and the
// caller's own preference (an API key's owner; a Worker identity has no user,
// so its preference is off). A setting that does not resolve counts as
// disabled (fail closed).
func hiddenSessionsPolicy(hubPolicy func() string, st *store.Store) workers.HiddenSessionsPolicyFunc {
	return func(ctx context.Context, p authz.Principal) (workers.HiddenSessionsPolicy, error) {
		pol := workers.HiddenSessionsPolicy{Hub: hubPolicy()}
		if pol.Hub != config.HiddenSessionsUserControlled || p.UserID == "" || p.Kind == authz.KindWorker {
			return pol, nil
		}
		prefs, err := st.UserPrefs(ctx, p.UserID)
		if err != nil {
			return pol, fmt.Errorf("route: read the preferences of the caller: %w", err)
		}
		pol.UserAllows = prefs.AllowHiddenSessions
		return pol, nil
	}
}

// complianceMonitor is the Hub's compliance monitor; nil without the log
// export module (the Router then records nothing).
func complianceMonitor(env *Env) *compliance.Monitor {
	if o, err := Require[*ObsService](env); err == nil {
		return o.Compliance
	}
	return nil
}
