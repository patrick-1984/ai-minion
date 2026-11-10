package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// shareModuleOrder places the share module after the events module (55), whose
// session state it keeps up to date, and before the pipe module (60), whose
// history it hides for a session that was unshared.
const shareModuleOrder = 56

func init() { registerShareModule(DefaultRegistry) }

// registerShareModule registers the module of Story 4.15: share_get and
// share_set on the REST handler registry, routed to the Workers by the router
// of the routing module, with the Hub's session state ([workers.EventHub]) as
// the fence that follows every answer.
func registerShareModule(r *Registry) {
	if err := r.Register("share", shareModuleOrder, initShare); err != nil {
		panic(err)
	}
}

func initShare(_ context.Context, env *Env) error {
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	rt, err := Require[*workers.Router](env)
	if err != nil {
		return err
	}
	hub, err := Require[*workers.EventHub](env)
	if err != nil {
		return err
	}
	if err := api.RegisterShareOps(handlers, rt, hub); err != nil {
		return fmt.Errorf("share: %w", err)
	}
	return nil
}
