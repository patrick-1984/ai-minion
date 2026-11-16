package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/updates"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// updatesModuleOrder runs after the settings (the check reads them live).
const updatesModuleOrder = 37

// releasesAPI is the fixed GitHub API root of the release check: it is not a
// setting, so the check can only ever reach GitHub.
const releasesAPI = "https://api.github.com"

func init() { registerUpdates(DefaultRegistry) }

func registerUpdates(r *Registry) {
	if err := r.Register("updates", updatesModuleOrder, initUpdates); err != nil {
		panic(err)
	}
}

func initUpdates(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	DeclareLiveSetting(config.KeyUpdateCheck)
	DeclareLiveSetting(config.KeyUpdateChannel)
	src, err := version.NewGitHubSource(releasesAPI, "minion-hub/"+version.Version)
	if err != nil {
		return err
	}
	resolver := settings.Resolver()
	chk := &updates.Checker{
		Source: src, Current: version.Version, Log: cfg.logger(),
		Interval: 24 * time.Hour, FirstDelay: 2 * time.Minute,
		Setting: func(key string) string {
			res, err := resolver.Resolve(key)
			if err != nil {
				return ""
			}
			return res.Value
		},
	}
	web.SetUpdateSource(func() (string, string, bool) {
		r := chk.Latest()
		return r.Latest, r.URL, r.Newer
	})
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var started atomic.Bool
	env.OnStart(func(context.Context) error {
		started.Store(true)
		go func() {
			defer close(done)
			chk.Run(runCtx)
		}()
		return nil
	})
	env.OnStop(func(ctx context.Context) error {
		cancel()
		web.SetUpdateSource(nil)
		if !started.Load() {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("the release check did not stop: %w", ctx.Err())
		}
	})
	return nil
}
