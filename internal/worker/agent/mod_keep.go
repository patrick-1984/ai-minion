package agent

import (
	"context"
	"os"

	"github.com/patrick-1984/ai-minion/internal/worker/keep"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Module name and start order of the keep list (session_keep_set, v0.24.0).
// It starts after the core module, whose store, host and executor it needs,
// and before the connector, so that the op and its feature are in the first
// hello.
const (
	// ModuleKeep saves and restores the kept sessions.
	ModuleKeep = "keep"
	// OrderKeep is its start order.
	OrderKeep = 160
)

func init() { RegisterModule(ModuleKeep, OrderKeep, startKeep) }

// keepProcRoot is the seam through which tests choose the process table.
var keepProcRoot = ""

// startKeep attaches the keep list to the executor and runs the keeper: the
// restore pass at the start (once the terminal host is up; on Android that is
// when the app service has started the managed tmux after a reboot with
// start-on-boot on), and the saves until the Worker stops. A host that cannot
// save and rebuild layouts (psmux, ConPTY) gets no keep list: the op is not
// announced.
func startKeep(ctx context.Context, env *Env) (StopFunc, error) {
	st, err := Require[*store.Store](env)
	if err != nil {
		return nil, err
	}
	host, err := Require[term.SessionHost](env)
	if err != nil {
		return nil, err
	}
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}
	log := env.Settings().logger()
	layouts, ok := term.KeepsLayouts(host)
	if !ok {
		log.Info("keep: this terminal host cannot keep sessions across restarts")
		return nil, nil
	}
	home, _ := os.UserHomeDir()
	managed := false
	if m, ok := host.(interface{ Managed() bool }); ok {
		managed = m.Managed()
	}
	k, err := keep.New(keep.Config{
		Host: host, Layouts: layouts, Store: st, Managed: managed, Home: home,
		ProcRoot: keepProcRoot, WorkerID: ex.policy.WorkerID, Logger: log,
	})
	if err != nil {
		return nil, err
	}
	ex.AttachKeep(k)
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.Run(runCtx)
	}()
	return func(context.Context) error {
		cancel()
		<-done
		return nil
	}, nil
}
