package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// accountsModuleOrder places the accounts module after the REST framework
// (40), whose handler registry it adds to.
const accountsModuleOrder = 45

func init() { registerAccounts(DefaultRegistry, ops.Default) }

// registerAccounts registers the account administration module in r for the
// operations of catalog (Story 3.10). It registers the handlers of users_list,
// user_create, user_reset_password, user_set_role, user_deactivate and
// password_change on the REST framework's handler registry and provides the
// *local.Admin service, so a later module (the API key store, Story 6.1) can
// set Admin.RevokeKeys during init, before the Hub serves. When the catalog
// has none of these operations (a test catalog) the module does nothing and
// provides nothing.
func registerAccounts(r *Registry, catalog *ops.Registry) {
	err := r.Register("accounts", accountsModuleOrder, func(ctx context.Context, env *Env) error {
		return initAccounts(ctx, env, catalog)
	}, DeclaresService[*local.Admin]())
	if err != nil {
		panic(err)
	}
}

func initAccounts(_ context.Context, env *Env, catalog *ops.Registry) error {
	if _, ok := catalog.Lookup(ops.OpUserCreate); !ok {
		return nil
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	limiter, err := Require[*ratelimit.Limiter](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	admin := &local.Admin{Store: st, Hasher: local.NewHasher(), Backoff: limiter}
	if err := api.RegisterAccounts(handlers, admin); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	return Provide(env, admin)
}
