package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// catalogModuleOrder places the catalog module after the api module (40),
// which provides the authenticator.
const catalogModuleOrder = 41

func init() { registerCatalogModule(DefaultRegistry) }

// registerCatalogModule registers the catalog module in r. It mounts
// GET /api/v1/ops, the OpenAPI description generated from the operation
// catalog, for any authenticated principal.
func registerCatalogModule(r *Registry) {
	if err := r.Register("catalog", catalogModuleOrder, initCatalog); err != nil {
		panic(err)
	}
}

func initCatalog(_ context.Context, env *Env) error {
	mux, err := Require[*http.ServeMux](env)
	if err != nil {
		return err
	}
	authn, err := Require[*api.Authenticator](env)
	if err != nil {
		return err
	}
	h, err := api.NewCatalogHandler(authn, ops.Default)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	mux.Handle(api.CatalogPath, h)
	return nil
}
