package server

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

// authzModuleOrder places the authorization module right after the store
// module (order 10) and before the modules that wire its lookups (auth,
// workers) and the modules that decide anything (API, ops router, web).
const authzModuleOrder = 15

func init() { registerAuthz(DefaultRegistry) }

// registerAuthz registers the authz module in r. It provides the one
// [*authz.Authorizer] of the Hub. The user lookup belongs to the auth module
// (Story 3.5) and the Worker lookup to the workers module (Story 4.3), so this
// module does not require them: the Authorizer starts with fail-closed
// placeholders and those modules wire it once each from their own init with
// [authz.Authorizer.SetUserLookup] and [authz.Authorizer.SetWorkerLookup]. Until
// both are wired every decision fails closed with [authz.ErrNotWired], and the
// Hub still starts.
func registerAuthz(r *Registry) {
	err := r.Register("authz", authzModuleOrder, func(_ context.Context, env *Env) error {
		return Provide(env, authz.NewUnwired())
	}, DeclaresService[*authz.Authorizer]())
	if err != nil {
		panic(err)
	}
}
