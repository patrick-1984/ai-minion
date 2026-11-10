package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// workersModuleOrder places the workers module after the store (10), authz
// (15) and audit (20) modules it requires.
const workersModuleOrder = 30

func init() { registerWorkersModule(DefaultRegistry) }

// registerWorkersModule registers the workers module in r. It requires the
// store, the authorizer, the audit log, the HTTP mux and the network helper,
// and provides the one [*workers.Service]: the Hub identity, the registry of
// online Workers and the Worker connect endpoint.
func registerWorkersModule(r *Registry) {
	if err := r.Register("workers", workersModuleOrder, initWorkers, DeclaresService[*workers.Service]()); err != nil {
		panic(err)
	}
}

// initWorkers loads or creates the Hub identity (logging its fingerprint),
// wires the Worker owner lookup into the authorizer, mounts GET
// /api/v1/worker/connect and closes every Worker connection at stop.
func initWorkers(ctx context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	nw, err := Require[*Network](env)
	if err != nil {
		return err
	}
	mux, err := Require[*http.ServeMux](env)
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
	auditLog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}

	svc, err := workers.NewService(ctx, workers.ServiceOptions{
		DataDir:  cfg.DataDir,
		Store:    st,
		ClientIP: nw.ClientIP,
		Auditor:  auditLog,
		Logger:   cfg.logger(),
	})
	if err != nil {
		return fmt.Errorf("start the workers service: %w", err)
	}
	if err := az.SetWorkerLookup(svc.OwnerLookup()); err != nil {
		return err
	}
	if err := Provide(env, svc); err != nil {
		return err
	}
	mux.Handle("GET "+transport.ConnectPath, svc.Handler())
	env.OnStop(func(context.Context) error {
		svc.Close()
		return nil
	})
	return nil
}
