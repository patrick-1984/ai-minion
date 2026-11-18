package agent

import (
	"context"
	"time"
)

// The log sinks module stops the remote log sinks (syslog, OpenTelemetry)
// that `minion worker run` built with the logger. It starts first and so
// stops last: the sinks carry the stop of every other module. Its share of
// the stop budget (DefaultStopTimeout) is LogSinksStopLimit, and it ends half
// a second before the module deadline.
const (
	ModuleLogSinks = "log_sinks"
	OrderLogSinks  = 10

	// LogSinksStopLimit bounds the flush of the sinks.
	LogSinksStopLimit = 2 * time.Second
)

func init() { RegisterModule(ModuleLogSinks, OrderLogSinks, startLogSinks) }

func startLogSinks(_ context.Context, env *Env) (StopFunc, error) {
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
		return stop(sctx)
	}, nil
}
