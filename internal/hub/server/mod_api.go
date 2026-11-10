package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// apiModuleOrder places the REST framework after the auth module (30), whose
// session manager, limiter and authorizer wiring it needs, and before the
// modules that register handlers and aliases on it (accounts, keys, workers,
// sessions, web).
const apiModuleOrder = 40

func init() { registerAPI(DefaultRegistry, ops.Default) }

// registerAPI registers the REST API module in r for the operations of
// catalog (Story 3.9). It
//
//   - mounts POST /api/v1/ops/<name> (and GET for read operations) and the
//     enveloped not_found for every other /api/v1 path on the root mux;
//   - authenticates browser sessions with CSRF and "Bearer mn_..." keys, with
//     the apikey_fail limiter per IP and an audit row for each failed bearer;
//   - provides *api.Registry, where later modules register the handlers of
//     catalog operations (api.HandleTyped), *api.Router, for resource
//     aliases, and *api.Authenticator, whose SetKeyAuthenticator the API-key
//     module (Story 6.1) calls. Until it does, no bearer token is valid;
//   - serves audit_list, which the audit module's log already implements,
//     and the alias GET /api/v1/audit.
func registerAPI(r *Registry, catalog *ops.Registry) {
	err := r.Register("api", apiModuleOrder, func(ctx context.Context, env *Env) error {
		return initAPI(ctx, env, catalog)
	},
		DeclaresService[*api.Registry](),
		DeclaresService[*api.Router](),
		DeclaresService[*api.Authenticator](),
	)
	if err != nil {
		panic(err)
	}
}

// requireHubWideScope refuses an API key whose scope does not reach the whole
// Hub. audit_list is decided on the key owner's own user resource, but it
// returns the events of every user, and AD-11 has the Hub filter list results
// by scope. An audit event names no resource a scope could be tested on, so a
// key may read the log only when its scope grants manage on a user it has
// never been given (the "everything" scope); a key limited to chosen users,
// Workers or sessions is forbidden. Browser sessions and act-as are not
// scoped, and the dispatcher has already required the owner to be an admin.
func requireHubWideScope(p authz.Principal) error {
	if p.Kind != authz.KindAPIKey {
		return nil
	}
	if p.Scope != nil {
		id := store.NewID() // a user no chosen-users scope can list
		probe := authz.ResolvedResource{Kind: authz.ResourceUser, UserID: id, OwnerID: id}
		if slices.Contains(p.Scope.Actions(probe), ops.ActionManage) {
			return nil
		}
	}
	return api.NewError(proto.CodeForbidden, "this API key is limited to some resources; reading the audit log takes an everything scope")
}

func initAPI(_ context.Context, env *Env, catalog *ops.Registry) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	mux, err := Require[*http.ServeMux](env)
	if err != nil {
		return err
	}
	nw, err := Require[*Network](env)
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
	limiter, err := Require[*ratelimit.Limiter](env)
	if err != nil {
		return err
	}
	mgr, err := Require[*session.Manager](env)
	if err != nil {
		return err
	}

	handlers := api.NewRegistry(catalog)
	if err := handlers.Merge(api.Default); err != nil {
		return fmt.Errorf("api: merge the default handlers: %w", err)
	}
	if _, ok := catalog.Lookup(ops.OpAuditList); ok && !slices.Contains(handlers.Names(), ops.OpAuditList) {
		err := api.HandleTyped(handlers, ops.OpAuditList, func(ctx context.Context, c *api.Call, args ops.AuditListArgs) (ops.AuditListResult, error) {
			if err := requireHubWideScope(c.Principal); err != nil {
				return ops.AuditListResult{}, err
			}
			return alog.ListOp(ctx, args)
		})
		if err != nil {
			return fmt.Errorf("api: register audit_list: %w", err)
		}
	}

	authn := &api.Authenticator{
		Sessions: mgr, Limiter: limiter, Audit: alog,
		ClientIP: nw.ClientIP, Log: cfg.logger(),
	}
	d := &api.Dispatcher{
		Catalog: catalog, Handlers: handlers, Auth: authn, Authz: az, Audit: alog, Log: cfg.logger(),
	}
	router, err := api.NewRouter(d, mux)
	if err != nil {
		return err
	}
	root, err := Require[*Root](env)
	if err != nil {
		return err
	}
	if err := root.Wrap(router.Handler); err != nil {
		return err
	}
	if _, ok := catalog.Lookup(ops.OpAuditList); ok {
		if err := router.AddAlias(api.Alias{Method: http.MethodGet, Pattern: "/api/v1/audit", Op: ops.OpAuditList}); err != nil {
			return err
		}
	}
	for _, p := range []func() error{
		func() error { return Provide(env, handlers) },
		func() error { return Provide(env, router) },
		func() error { return Provide(env, authn) },
	} {
		if err := p(); err != nil {
			return err
		}
	}
	return nil
}
