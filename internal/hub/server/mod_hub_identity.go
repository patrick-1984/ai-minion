package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// hubIdentityModuleOrder places the module after the REST framework (40),
// whose handler registry it fills, and after the workers module (30), whose
// rotator it exposes.
const hubIdentityModuleOrder = 47

func init() { registerHubIdentityModule(DefaultRegistry) }

// registerHubIdentityModule registers the Hub identity module in r (Story
// 4.9): it registers the handler of hub_identity_rotate.
func registerHubIdentityModule(r *Registry) {
	if err := r.Register("hub_identity", hubIdentityModuleOrder, initHubIdentity); err != nil {
		panic(err)
	}
}

func initHubIdentity(_ context.Context, env *Env) error {
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	rot := svc.Rotator()
	if rot == nil {
		return errors.New("the workers service has no identity rotator")
	}
	if err := api.RegisterHubIdentityHandlers(handlers, rot); err != nil {
		return fmt.Errorf("register the Hub identity handler: %w", err)
	}
	return nil
}
