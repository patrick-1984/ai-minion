package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/selfupdate"
	"github.com/patrick-1984/ai-minion/internal/hub/updates"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// selfUpdateModuleOrder runs after the REST framework (40), the workers (30)
// and the release check (37).
const selfUpdateModuleOrder = 49

func init() {
	DeclareLiveSetting(config.KeySelfUpdate)
	if err := DefaultRegistry.Register("self-update", selfUpdateModuleOrder, initSelfUpdate); err != nil {
		panic(err)
	}
}

// selfUpdater is the Hub's own update (v0.9.0): hub_update_check and
// hub_update.
type selfUpdater struct {
	dataDir  string
	setting  func(string) (string, error)
	checker  *updates.Checker
	docker   *selfupdate.Docker
	socket   string
	verified func(ctx context.Context, sel version.Selector) (version.Release, version.Manifest, error)
	online   func() []workers.Online
}

func initSelfUpdate(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	chk, err := Require[*updates.Checker](env)
	if err != nil {
		return err
	}
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	resolver := settings.Resolver()
	cache := filepath.Join(cfg.DataDir, "update-cache")
	u := &selfUpdater{
		dataDir: cfg.DataDir, checker: chk, socket: selfupdate.DefaultSocket,
		docker: selfupdate.NewDocker(selfupdate.DefaultSocket),
		setting: func(key string) (string, error) {
			r, err := resolver.Resolve(key)
			return r.Value, err
		},
		online: svc.Registry().List,
		verified: func(ctx context.Context, sel version.Selector) (version.Release, version.Manifest, error) {
			src, err := version.NewGitHubSource(releasesAPI, "minion-hub/"+version.Version)
			if err != nil {
				return version.Release{}, version.Manifest{}, err
			}
			up := &version.Updater{Source: src,
				Verifier: version.SigstoreVerifierFactory(version.TUFTrustedRoot{Dir: cache}),
				Cache:    version.Cache{Dir: cache}}
			return up.VerifiedRelease(ctx, sel)
		},
	}
	if err := api.HandleTyped(handlers, ops.OpHubUpdateCheck, func(ctx context.Context, _ *api.Call, _ ops.HubUpdateCheckArgs) (ops.HubUpdateCheckResult, error) {
		return u.check(ctx), nil
	}); err != nil {
		return fmt.Errorf("self-update: %w", err)
	}
	return api.HandleTyped(handlers, ops.OpHubUpdate, func(ctx context.Context, c *api.Call, a ops.HubUpdateArgs) (ops.HubUpdateResult, error) {
		if c.Principal.Kind == authz.KindAPIKey {
			return ops.HubUpdateResult{}, api.NewError(proto.CodeForbidden, "replacing the Hub is done by an administrator in the web UI, not with an API key")
		}
		return u.update(ctx, a.Force)
	})
}

// manualCommand is what to run when the Hub cannot replace itself.
const manualCommand = "Docker Compose: set MINION_VERSION (or MINION_TAG) in .env, then docker compose pull && docker compose up -d. Binary: minion update, then restart the Hub service."

func (u *selfUpdater) check(ctx context.Context) ops.HubUpdateCheckResult {
	res := ops.HubUpdateCheckResult{Current: version.Version}
	res.Channel, _ = u.setting(config.KeyUpdateChannel)
	l := u.checker.Latest()
	res.Latest, res.Newer, res.ReleaseURL = l.Latest, l.Newer, l.URL
	if mode, err := u.setting(config.KeySelfUpdate); err != nil || mode != "auto" {
		res.Reason, res.Command = "self_update is off", manualCommand
		return res
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, why := selfupdate.Detect(dctx, u.docker, u.socket); why != "" {
		res.Reason, res.Command = why, manualCommand
		return res
	}
	res.SelfUpdate = true
	return res
}

func (u *selfUpdater) update(ctx context.Context, force bool) (ops.HubUpdateResult, error) {
	res := ops.HubUpdateResult{From: version.Version}
	chk := u.check(ctx)
	if !chk.SelfUpdate {
		res.Reason = chk.Reason
		return res, nil
	}
	cur, err := version.ParseVersion(version.Version)
	if err != nil {
		res.Reason = "this Hub is a development build"
		return res, nil
	}
	sel, err := updates.SelectorFor(chk.Channel, cur, true)
	if err != nil {
		return res, err
	}
	vctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rel, man, err := u.verified(vctx, sel)
	if err != nil {
		return res, api.NewError(proto.CodeConflict, "the release could not be verified: "+err.Error())
	}
	res.To = rel.Tag
	target, err := version.ParseVersion(rel.Tag)
	if err != nil || target.Compare(cur) <= 0 {
		res.Reason = "no newer release in the channel " + chk.Channel
		return res, nil
	}
	if man.Image == nil {
		res.Reason = "the release " + rel.Tag + " names no image digest (released before 0.9.0)"
		return res, nil
	}
	// The Hub must keep speaking with the connected Workers: refuse a release
	// whose protocol range leaves some of them out, unless forced.
	rng, err := man.Range()
	if err != nil {
		return res, err
	}
	for _, w := range u.online() {
		pv, err := version.ParseProtoVersion(w.Version.String())
		if err == nil && (pv.Compare(rng.Min) < 0 || pv.Compare(rng.Max) > 0) {
			res.Stranded = append(res.Stranded, w.Name)
		}
	}
	if len(res.Stranded) > 0 && !force {
		res.Reason = fmt.Sprintf("%d connected Workers speak a protocol %s does not support; update them first or force", len(res.Stranded), rel.Tag)
		return res, nil
	}
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Minute)
	defer dcancel()
	self, why := selfupdate.Detect(dctx, u.docker, u.socket)
	if why != "" {
		res.Reason = why
		return res, nil
	}
	if _, err := selfupdate.StartHelper(dctx, u.docker, self, man.Image.Ref(), version.Version); err != nil {
		if errors.Is(err, selfupdate.ErrBusy) {
			res.Reason = err.Error()
			return res, nil
		}
		return res, api.NewError(proto.CodeConflict, "the update could not start: "+err.Error())
	}
	res.Started = true
	return res, nil
}
