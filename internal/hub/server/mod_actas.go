package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
)

// actAsModuleOrder places the act-as module after the REST framework (40),
// whose handler registry it fills, and before the web module (80), whose
// banner it feeds.
const actAsModuleOrder = 45

func init() { registerActAs(DefaultRegistry) }

// registerActAs registers the act-as module in r (Story 6.5, R45). It serves
// act_as_start and act_as_stop on the REST API with the session manager of the
// auth module. The state of an act-as session lives in the same database as
// the browser session; the manager resolves it on every request, so no other
// module has to know about it.
func registerActAs(r *Registry) {
	err := r.Register("actas", actAsModuleOrder, initActAs)
	if err != nil {
		panic(err)
	}
}

func initActAs(_ context.Context, env *Env) error {
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	mgr, err := Require[*session.Manager](env)
	if err != nil {
		return err
	}
	if err := api.RegisterActAs(handlers, mgr); err != nil {
		return fmt.Errorf("act-as: %w", err)
	}
	return nil
}
