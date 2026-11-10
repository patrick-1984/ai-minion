package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// apikeyModuleOrder places the API-key module after the REST framework (40),
// whose authenticator and handler registry it fills, and after the auth (30)
// and workers (30) modules that wire the lookups of the authorizer.
const apikeyModuleOrder = 45

func init() { registerAPIKeyModule(DefaultRegistry) }

// registerAPIKeyModule registers the API-key module in r (Story 6.1). It
//
//   - provides the one [*apikey.Service] of the Hub;
//   - wires it as the [api.KeyAuthenticator] of the REST framework, so that
//     "Authorization: Bearer mn_..." keys authenticate. Until this module
//     runs no bearer token is valid (fail closed);
//   - registers the handlers of apikey_create, apikey_list and apikey_revoke.
func registerAPIKeyModule(r *Registry) {
	if err := r.Register("apikey", apikeyModuleOrder, initAPIKey, DeclaresService[*apikey.Service]()); err != nil {
		panic(err)
	}
}

func initAPIKey(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	authn, err := Require[*api.Authenticator](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	svc, err := apikey.New(apikey.Config{Repo: st, Authz: az, Users: userLookup{st: st}, Log: cfg.logger()})
	if err != nil {
		return err
	}
	if err := authn.SetKeyAuthenticator(keyAuthenticator{svc: svc}); err != nil {
		return fmt.Errorf("wire the key authenticator: %w", err)
	}
	if err := api.RegisterAPIKeyHandlers(handlers, svc); err != nil {
		return fmt.Errorf("register the API key handlers: %w", err)
	}
	return Provide(env, svc)
}

// keyAuthenticator adapts the key service to the REST framework's port: the
// service's invalid-key error becomes api.ErrInvalidKey, the only error that
// counts as a failed attempt.
type keyAuthenticator struct{ svc *apikey.Service }

// AuthenticateKey implements api.KeyAuthenticator.
func (k keyAuthenticator) AuthenticateKey(ctx context.Context, token string) (api.KeyIdentity, error) {
	id, err := k.svc.AuthenticateKey(ctx, token)
	if errors.Is(err, apikey.ErrInvalidKey) {
		return api.KeyIdentity{}, api.ErrInvalidKey
	}
	if err != nil {
		return api.KeyIdentity{}, err
	}
	return api.KeyIdentity{KeyID: id.KeyID, Principal: id.Principal}, nil
}
