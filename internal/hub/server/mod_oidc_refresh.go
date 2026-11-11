package server

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/oidc"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// oidcRefreshModuleOrder places the refresh module after the OIDC module (35)
// whose providers it refreshes.
const oidcRefreshModuleOrder = 36

func init() {
	// The interval is resolved again after every pass, so a panel change
	// applies without a restart.
	DeclareLiveSetting(config.KeyOIDCRefreshInterval)
	registerOIDCRefresh(DefaultRegistry, nil)
}

// registerOIDCRefresh registers the OIDC background refresh module in r (Story
// 6.3). The module starts a refresher when the Hub starts and stops it, and
// waits for it, before the store closes. tune, when not nil, adjusts the
// refresher before it starts (tests).
func registerOIDCRefresh(r *Registry, tune func(*oidc.Refresher)) {
	err := r.Register("oidc-refresh", oidcRefreshModuleOrder, func(ctx context.Context, env *Env) error {
		return initOIDCRefresh(ctx, env, tune)
	})
	if err != nil {
		panic(err)
	}
}

func initOIDCRefresh(_ context.Context, env *Env, tune func(*oidc.Refresher)) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	alog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	log := cfg.logger()
	resolver := settings.Resolver()

	ref := &oidc.Refresher{
		// The providers come from the same source as the sign-in: the settings
		// and the panel, resolved on every pass.
		Configs:  &providerSource{resolver: resolver, store: st, log: log},
		Store:    &oidc.SQLStore{Store: st, Log: log},
		Audit:    alog,
		Log:      log,
		Interval: oidc.IntervalFromResolver(resolver),
	}
	if tune != nil {
		tune(ref)
	}
	// A promotion to IdP admin at sign-in wakes the refresher (F-07).
	if svc, err := Require[*oidc.Service](env); err == nil {
		svc.AdminGranted = ref.Wake
	}

	// The refresher outlives the start context, so it has a context of its own
	// that the stop hook cancels.
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var started atomic.Bool
	env.OnStart(func(context.Context) error {
		started.Store(true)
		go func() {
			defer close(done)
			if err := ref.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("oidc refresh: the background refresh stopped", "error", err)
				return
			}
			log.Info("oidc refresh: background refresh stopped")
		}()
		return nil
	})
	env.OnStop(func(ctx context.Context) error {
		cancel()
		if !started.Load() {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("the OIDC background refresh did not stop: %w", ctx.Err())
		}
	})
	return nil
}
