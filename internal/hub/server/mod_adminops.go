package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// adminOpsModuleOrder places the module after the REST framework (40), whose
// handler registry it fills, and after the OIDC module (35) and the settings
// module (20), whose services it binds. The web module (80) connects the
// administration pages to the same operations.
const adminOpsModuleOrder = 46

func init() { registerAdminOps(DefaultRegistry, ops.Default) }

// registerAdminOps registers the module that binds the settings and OIDC
// services to the REST handler registry (Story 6.8): settings_list and
// settings_set to the settings service, and oidc_provider_list,
// oidc_provider_set and role_rule_set to the OIDC handlers. The administration
// pages call these operations through the REST pipeline, so without this
// module they could not list or change anything. An operation that the catalog
// lacks (a test catalog) is not registered.
func registerAdminOps(r *Registry, catalog *ops.Registry) {
	err := r.Register("adminops", adminOpsModuleOrder, func(ctx context.Context, env *Env) error {
		return initAdminOps(ctx, env, catalog)
	})
	if err != nil {
		panic(err)
	}
}

func catalogHas(catalog *ops.Registry, names ...string) bool {
	for _, n := range names {
		if _, ok := catalog.Lookup(n); !ok {
			return false
		}
	}
	return true
}

func initAdminOps(_ context.Context, env *Env, catalog *ops.Registry) error {
	wantSettings := catalogHas(catalog, ops.OpSettingsList, ops.OpSettingsSet)
	wantOIDC := catalogHas(catalog, ops.OpOIDCProviderList, ops.OpOIDCProviderSet, ops.OpRoleRuleSet)
	if !wantSettings && !wantOIDC {
		return nil
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	if wantSettings {
		svc, err := Require[*SettingsService](env)
		if err != nil {
			return err
		}
		if err := registerSettingsOps(handlers, svc); err != nil {
			return fmt.Errorf("adminops: %w", err)
		}
	}
	if wantOIDC {
		h, err := Require[*api.OIDCHandlers](env)
		if err != nil {
			return err
		}
		if err := registerOIDCOps(handlers, h); err != nil {
			return fmt.Errorf("adminops: %w", err)
		}
	}
	return nil
}

// registerSettingsOps binds settings_list and settings_set. The dispatcher has
// decided manage on the caller's own user resource (admins only) before a
// handler runs. Like users_list, the settings are Hub-wide, so an API key that
// is limited to some resources is refused.
func registerSettingsOps(r *api.Registry, svc *SettingsService) error {
	if err := api.HandleTyped(r, ops.OpSettingsList, func(ctx context.Context, c *api.Call, a ops.SettingsListArgs) (ops.SettingsListResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.SettingsListResult{}, err
		}
		return svc.List(ctx, a)
	}); err != nil {
		return err
	}
	return api.HandleTyped(r, ops.OpSettingsSet, func(ctx context.Context, c *api.Call, a ops.SettingsSetArgs) (ops.WriteResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.WriteResult{}, err
		}
		return svc.Set(ctx, SettingsActor{ID: c.Identity.Actor, ActingAs: c.Identity.ActingAs}, a)
	})
}

// registerOIDCOps binds oidc_provider_list, oidc_provider_set and
// role_rule_set, with the same Hub-wide scope rule as the settings.
func registerOIDCOps(r *api.Registry, h *api.OIDCHandlers) error {
	actor := func(c *api.Call) api.OIDCActor {
		return api.OIDCActor{ID: c.Identity.Actor, ActingAs: c.Identity.ActingAs}
	}
	if err := api.HandleTyped(r, ops.OpOIDCProviderList, func(ctx context.Context, c *api.Call, a ops.OIDCProviderListArgs) (ops.OIDCProviderListResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.OIDCProviderListResult{}, err
		}
		return h.ProviderList(ctx, a)
	}); err != nil {
		return err
	}
	if err := api.HandleTyped(r, ops.OpOIDCProviderSet, func(ctx context.Context, c *api.Call, a ops.OIDCProviderSetArgs) (ops.WriteResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.WriteResult{}, err
		}
		return h.ProviderSet(ctx, actor(c), a)
	}); err != nil {
		return err
	}
	return api.HandleTyped(r, ops.OpRoleRuleSet, func(ctx context.Context, c *api.Call, a ops.RoleRuleSetArgs) (ops.WriteResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.WriteResult{}, err
		}
		return h.RoleRuleSet(ctx, actor(c), a)
	})
}
