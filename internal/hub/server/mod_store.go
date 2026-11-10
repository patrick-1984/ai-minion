package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// OrderStore is the init order of the store module. It comes right after the
// core module so that every later module can [Require] the database.
const OrderStore = 10

// storeModuleName is the name of the module that opens the Hub database.
const storeModuleName = "store"

func init() {
	if err := registerStore(DefaultRegistry); err != nil {
		panic(err)
	}
}

// registerStore adds the store module to r.
func registerStore(r *Registry) error {
	return r.Register(storeModuleName, OrderStore, initStore, DeclaresService[*store.Store]())
}

// initStore opens the Hub database in the data dir (applying the pending
// migrations), provides it as *store.Store, closes it at stop and adds the
// readiness check "database".
func initStore(ctx context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	health, err := Require[*Health](env)
	if err != nil {
		return err
	}
	st, err := store.OpenContext(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	if err := Provide(env, st); err != nil {
		_ = st.Close()
		return err
	}
	env.OnStop(func(context.Context) error { return st.Close() })
	health.AddReadyCheck("database", func(ctx context.Context) error { return checkStore(ctx, st) })
	return nil
}

// checkStore is the readiness check "database": the database answers.
//
// It does not query the schema. SQL lives only in the store packages (AD-16),
// and "migrations applied" holds by construction: [store.OpenContext] applies
// the pending migrations synchronously and fails otherwise, so initStore never
// gets a *store.Store with unapplied migrations, and [Health.MarkStarted] runs
// only after [Kernel.Start] returned, that is after every init function.
func checkStore(ctx context.Context, st *store.Store) error {
	if err := st.DB().PingContext(ctx); err != nil {
		return fmt.Errorf("database does not answer: %w", err)
	}
	return nil
}
