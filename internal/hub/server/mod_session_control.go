package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// sessionControlModuleOrder places the module after the REST framework (40),
// whose handler registry it fills.
const sessionControlModuleOrder = 46

func init() { registerSessionControlModule(DefaultRegistry, ops.Default) }

// registerSessionControlModule registers the module that binds
// session_control_list and session_control_set to the store. An operation
// that the catalog lacks (a test catalog) is not registered.
func registerSessionControlModule(r *Registry, catalog *ops.Registry) {
	err := r.Register("session_control", sessionControlModuleOrder, func(_ context.Context, env *Env) error {
		if !catalogHas(catalog, ops.OpSessionControlList, ops.OpSessionControlSet) {
			return nil
		}
		handlers, err := Require[*api.Registry](env)
		if err != nil {
			return err
		}
		st, err := Require[*store.Store](env)
		if err != nil {
			return err
		}
		if err := api.RegisterSessionControl(handlers, st); err != nil {
			return fmt.Errorf("session_control: %w", err)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
}
