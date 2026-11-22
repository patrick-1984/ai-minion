package agent

import (
	"context"
	"os"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
)

// Module name and start order of the presence publisher. It starts after every
// other module, so the record says "running" only once the Worker is up, and
// it stops first, so the stopped marker is written before the rest shuts down.
const (
	// ModulePresence publishes the local presence record (A79).
	ModulePresence = "presence"
	// OrderPresence is its start order.
	OrderPresence = 900
)

func init() { RegisterModule(ModulePresence, OrderPresence, startPresence) }

// presenceLayout is the seam through which tests choose where the module
// publishes (an injected temporary root).
var presenceLayout = presence.DefaultLayout

// startPresence publishes the presence record of this daemon. A failure to
// publish never keeps the Worker from running: it is logged and the Worker
// runs without a record (readers then see it as unavailable).
func startPresence(ctx context.Context, env *Env) (StopFunc, error) {
	s := env.Settings()
	log := s.logger()
	if s.StateDir == "" {
		return nil, nil
	}
	layout, err := presenceLayout(s.StateDir)
	if err != nil {
		log.Warn("the presence record is not published", "error", err)
		return nil, nil
	}
	pub, err := presence.New(presence.Options{
		Layout: layout, Instance: env.rt.Instance(), Version: version.Version,
		StartedAt: env.rt.StartedAt(), PID: os.Getpid(), Logger: log,
		HubOrigin: func() string {
			if s.Resolver == nil {
				return ""
			}
			wh, err := config.ResolveWorkerHub(s.Resolver)
			if err != nil {
				return ""
			}
			return wh.Server
		},
	})
	if err == nil {
		err = pub.Start(ctx)
	}
	if err != nil {
		log.Warn("the presence record is not published", "error", err)
		return nil, nil
	}
	if err := Provide(env, pub); err != nil {
		_ = pub.Stop()
		return nil, err
	}
	// A revoke, a reset or a new identity ends the authority of whatever usage
	// document was stored: it goes from memory and from disk.
	if enr, ok := lookupService[*Enrollments](env.rt); ok && enr != nil {
		enr.SetWipeHook(func() {
			if err := pub.ClearUsage(); err != nil {
				log.Warn("the stored usage document could not be removed", "error", err)
			}
		})
	}
	return func(context.Context) error { return pub.Stop() }, nil
}
