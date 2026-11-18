package agent

import (
	"context"
	"time"
)

// The log sinks module starts and stops the remote log sinks (syslog,
// OpenTelemetry) of `minion worker run`. It starts right after the core
// module, so after the lifecycle lock and the update guard (a sink problem
// can never decide whether an updated binary counts as started) and after
// the pre-start hooks (an enrollment gives the sinks the Worker id); a sink
// problem never fails the start. It stops before the core: its share of the
// stop budget (DefaultStopTimeout) is LogSinksStopLimit, and it ends half a
// second before the module deadline.
const (
	ModuleLogSinks = "log_sinks"
	OrderLogSinks  = OrderCore + 5

	// LogSinksStopLimit bounds the flush of the sinks.
	LogSinksStopLimit = 2 * time.Second
)

func init() { RegisterModule(ModuleLogSinks, OrderLogSinks, startLogSinks) }

func startLogSinks(ctx context.Context, env *Env) (StopFunc, error) {
	if start := env.Settings().StartLogSinks; start != nil {
		start(ctx)
	}
	stop := env.Settings().StopLogSinks
	if stop == nil {
		return nil, nil
	}
	return func(ctx context.Context) error {
		d := LogSinksStopLimit
		if dl, ok := ctx.Deadline(); ok {
			d = min(d, max(time.Until(dl)-500*time.Millisecond, 0))
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
		defer cancel()
		// Records the sinks could not send are written to the local log
		// by the sinks; that loss is expected (a dead collector) and must
		// not turn a clean stop into a failure (or a self-update into a
		// detached restart).
		_ = stop(sctx)
		return nil
	}, nil
}
