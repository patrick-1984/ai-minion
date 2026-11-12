package server

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// workersRemoveModuleOrder places the removal module after the workers (30),
// REST (40) and API-key (45) modules whose services and registry it uses.
const workersRemoveModuleOrder = 50

func init() { registerWorkersRemoveModule(DefaultRegistry) }

// registerWorkersRemoveModule registers the module of Story 4.10 in r. It
// requires the store, the audit log, the workers service and the REST handler
// registry, builds the one [*workers.Remover] of the Hub and binds the
// operation worker_remove to it.
//
// The removal transaction runs these steps with the tombstone:
//
//   - the API keys bound to the Worker are revoked (store.RevokeAPIKeysBoundToWorker);
//   - every trust row of the Worker and its sessions is deleted, for every
//     receiver (store.DeleteTrustOfWorker, Story 6.6);
//   - the ok audit record is written.
//
// The signed revoke goes to an online Worker through the registry of the
// workers service.
//
// The module also installs the removal behind the worker_retire call of a
// Worker (Story 4.7, amendment A-16) with [workers.Service.SetRemoveWorker]: the
// same transaction steps, the ok audit record inside the transaction, and no
// live revoke, because the call ends the connection itself. A Worker that is
// already removed is the idempotent success of that call.
func registerWorkersRemoveModule(r *Registry) {
	if err := r.Register("workers_remove", workersRemoveModuleOrder, initWorkersRemove); err != nil {
		panic(err)
	}
}

func initWorkersRemove(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	auditLog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	cleanup := func(live workers.LiveRevoker) workers.RemoveConfig {
		return workers.RemoveConfig{
			Store:   st,
			Auditor: auditLog,
			Live:    live,
			Keys: workers.CleanupFunc(func(ctx context.Context, tx *sql.Tx, workerID string) error {
				_, err := st.RevokeAPIKeysBoundToWorker(ctx, tx, workerID, store.UnixMS(time.Now()))
				return err
			}),
			Trust: workers.CleanupFunc(func(ctx context.Context, tx *sql.Tx, workerID string) error {
				_, err := st.DeleteTrustOfWorker(ctx, tx, workerID)
				return err
			}),
			Logger: cfg.logger(),
		}
	}
	rm, err := workers.NewRemover(cleanup(&workers.RegistryRevoker{Registry: svc.Registry()}))
	if err != nil {
		return fmt.Errorf("build the Worker remover: %w", err)
	}
	retire, err := workers.NewRemover(cleanup(nil))
	if err != nil {
		return fmt.Errorf("build the Worker retirement: %w", err)
	}
	svc.SetRemoveWorker(retireRemoval(retire), true)
	if err := api.RegisterWorkerRemove(handlers, &api.WorkerRemoveHandler{Remover: rm}); err != nil {
		return fmt.Errorf("register worker_remove: %w", err)
	}
	return nil
}

// retireRemoval adapts the remover to the removal function of the worker_retire
// call. The actor is the retiring Worker ("worker:<id>"). The Remover writes the
// ok audit record in its transaction; not_found (removed meanwhile, or by a
// duplicate call) is the success of an idempotent retirement.
func retireRemoval(rm *workers.Remover) workers.RemoveWorkerFunc {
	return func(ctx context.Context, workerID, removedBy string, _ int64) error {
		_, err := rm.Remove(ctx, workers.RemoveActor{ID: removedBy}, workerID)
		if proto.CodeOf(err) == proto.CodeNotFound {
			return nil
		}
		return err
	}
}
