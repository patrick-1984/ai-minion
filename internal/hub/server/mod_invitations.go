package server

import (
	"context"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// invitationsModuleOrder places the invitations module after the workers
// module (30), whose identity and connect handler it needs, the auth module
// (30), whose shared limiter it uses, and the API module (40), whose handler
// registry it extends.
const invitationsModuleOrder = 45

func init() { registerInvitationsModule(DefaultRegistry) }

// registerInvitationsModule registers the invitations module in r (Story
// 4.5). It
//
//   - resolves the infrastructure enrollment settings
//     (MINION_INFRA_INVITE_KEY_FILE, infra_default_owner) and loads the
//     Hub-wide infrastructure key, if one is configured;
//   - builds the invitation service and registers invitation_create,
//     invitation_list and invitation_revoke on the REST handler registry;
//   - builds the enroller (shared limiter, audit log, infrastructure key) and
//     registers it as the handler of the enroll_invite mode of the Worker
//     connect endpoint.
func registerInvitationsModule(r *Registry) {
	err := r.Register("invitations", invitationsModuleOrder, initInvitations,
		DeclaresService[*workers.Invitations](),
		DeclaresService[*workers.Enroller](),
	)
	if err != nil {
		panic(err)
	}
}

func initInvitations(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	alog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	limiter, err := Require[*ratelimit.Limiter](env)
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

	infra, err := config.ResolveHubInfra(settings.Resolver())
	if err != nil {
		return fmt.Errorf("invitations settings: %w", err)
	}
	fpr := svc.Identity().Fingerprint // the ROOT fingerprint, read at each use
	var infraKey *workers.InfraKey
	if infra.InviteKeyFile != "" {
		// An unreadable or malformed key file stops the start: a Hub that
		// silently ignored it would reject every infrastructure Worker.
		if fpr() == "" {
			return fmt.Errorf("invitations: MINION_INFRA_INVITE_KEY_FILE names a key bound to the Hub identity, but the Hub has no root key yet: remove (unset) MINION_INFRA_INVITE_KEY_FILE, start the Hub, set the Hub passphrase, then set MINION_INFRA_INVITE_KEY_FILE again and restart")
		}
		if infraKey, err = workers.LoadInfraKey(infra.InviteKeyFile, fpr()); err != nil {
			return fmt.Errorf("invitations: %w", err)
		}
	}

	inv, err := workers.NewInvitations(workers.InvitationsConfig{
		Store: st, FingerprintFunc: fpr, Auditor: alog, Logger: cfg.logger(),
	})
	if err != nil {
		return fmt.Errorf("invitations: %w", err)
	}
	if err := api.RegisterInvitationHandlers(handlers, inv, az); err != nil {
		return fmt.Errorf("invitations: register the API handlers: %w", err)
	}
	enr, err := workers.NewEnroller(workers.EnrollConfig{
		Store: st, FingerprintFunc: fpr, Limiter: limiter, Auditor: alog,
		Infra: infraKey, InfraOwner: infra.DefaultOwner, InfraExpiry: infraExpiry(infra), Logger: cfg.logger(),
	})
	if err != nil {
		return fmt.Errorf("invitations: %w", err)
	}
	if err := workers.RegisterEnroller(svc.Handler(), enr); err != nil {
		return fmt.Errorf("invitations: register the enroll_invite handler: %w", err)
	}
	if err := Provide(env, inv); err != nil {
		return err
	}
	return Provide(env, enr)
}

// infraExpiry maps infra_invite_key_days to the lifetime the enroller gives a
// new key file row: 0 days means no expiry.
func infraExpiry(infra config.HubInfra) time.Duration {
	if infra.InviteKeyLifetime <= 0 {
		return workers.NoInfraExpiry
	}
	return infra.InviteKeyLifetime
}
