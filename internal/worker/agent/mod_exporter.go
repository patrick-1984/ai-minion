package agent

import "context"

// The exporter module runs the Claude exporter installer inside the locked
// runtime: after the lifecycle lock and the core start, and it is stopped and
// joined before either is released, so no profile edit outlives the lock.
const (
	ModuleExporter = "exporter"
	OrderExporter  = OrderCore + 6
)

func init() { RegisterModule(ModuleExporter, OrderExporter, startExporter) }

func startExporter(ctx context.Context, env *Env) (StopFunc, error) {
	start := env.Settings().StartBackground
	if start == nil {
		return nil, nil
	}
	stop := start(ctx)
	if stop == nil {
		return nil, nil
	}
	return func(context.Context) error { stop(); return nil }, nil
}
