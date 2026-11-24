package oidc

// Silent renewal of browser sessions (owner ask A99).
//
// A browser session that was started with OIDC ends its idle lifetime like
// any other (the session_ttl setting, default 2 h). Instead of sending the
// person to the sign-in page, the session middleware then asks
// [Refresher.RenewSession]: the Hub runs the refresh-token grant with the
// refresh token it already stores for the account (the same encrypted token
// the background role refresh uses, see refresh.go), re-evaluates the roles
// with the configured rules and applies them in the same transaction as the
// background refresh does. Only when the IdP vouches for the user with fresh
// claims and the account keeps an IdP role does the session go on.
//
// The token never leaves the Hub: it is read from the store, sent to the
// token endpoint and, when the IdP rotates it, sealed again. No response,
// log line or audit row carries it.

import (
	"context"
	"errors"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

// Audit vocabulary of the silent session renewal (AD-20). The actor is the
// user; the target is the user name. Outcomes are [OutcomeSuccess],
// [OutcomeFailed] and [OutcomeRenewUnavailable].
const (
	AuditActionSessionRenew = "session.renew"
	// OutcomeRenewUnavailable records that the identity provider could not
	// be reached, so nothing was decided.
	OutcomeRenewUnavailable = "unavailable"
)

// SessionTargets is the optional port of a [RefreshStore] that finds the
// refresh target of one account; [SQLStore] offers it. Without it no session
// is renewed.
type SessionTargets interface {
	// RefreshTargetOf returns the target of the user at the provider, and
	// false when the user is inactive or holds no refresh token there.
	RefreshTargetOf(ctx context.Context, userID, providerID string) (Target, bool, error)
}

var _ SessionTargets = (*SQLStore)(nil)

var _ session.Renewer = (*Refresher)(nil)

// RefreshTargetOf implements [SessionTargets].
func (s *SQLStore) RefreshTargetOf(ctx context.Context, userID, providerID string) (Target, bool, error) {
	x, err := s.Store.OIDCRefreshTargetOf(ctx, userID, providerID)
	if errors.Is(err, store.ErrNotFound) {
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, err
	}
	return Target{
		IdentityID: x.IdentityID, UserID: x.UserID, Issuer: x.Issuer, Subject: x.Subject, ProviderID: x.ProviderID,
		LastLoginAt: x.LastLoginAt, RefreshToken: logx.Secret(x.RefreshToken), IdPAdmin: x.IdPAdmin,
	}, true, nil
}

// RenewSession implements [session.Renewer]: it asks the identity provider
// whether it still vouches for the user of the expired session s.
//
//   - [session.RenewOK]: the refresh returned fresh claims for the same
//     identity, the configured rules still give the account an IdP role, and
//     the result was stored (rotated token, roles).
//   - [session.RenewUnavailable]: the provider or its discovery could not be
//     reached, or answered something that proves nothing either way. Nothing
//     changes; a rotated token is still kept.
//   - [session.RenewFailed]: everything else, among them a rejected refresh
//     token (the IdP roles are suspended as by the background refresh), no
//     stored token, a provider that is gone, disabled or has another issuer,
//     and an account that no rule gives a role any more.
//
// Each attempt leaves one audit row.
func (r *Refresher) RenewSession(ctx context.Context, s *session.Session) session.RenewOutcome {
	out, why := r.renewSession(ctx, s)
	outcome := OutcomeFailed
	switch out {
	case session.RenewOK:
		outcome = OutcomeSuccess
		r.logger().Info("oidc: session renewed silently with the identity provider", "session", s.ID, "user", s.UserID)
	case session.RenewUnavailable:
		outcome = OutcomeRenewUnavailable
		r.logger().Warn("oidc: session renewal: identity provider not reachable", "session", s.ID, "user", s.UserID, "reason", why)
	default:
		r.logger().Warn("oidc: session renewal refused; the session ends", "session", s.ID, "user", s.UserID, "reason", why)
	}
	if err := r.Audit.Record(context.WithoutCancel(ctx), audit.Event{
		Actor: s.UserID, Action: AuditActionSessionRenew, Target: sanitizeTarget(s.Username), Outcome: outcome,
	}); err != nil {
		r.logger().Error("audit of a session renewal failed", "outcome", outcome, "error", err)
	}
	return out
}

// renewSession is [Refresher.RenewSession] without the audit; why is a short
// reason for the log (never a token or an IdP answer).
func (r *Refresher) renewSession(ctx context.Context, s *session.Session) (session.RenewOutcome, string) {
	if s == nil || !strings.HasPrefix(s.AuthMethod, session.AuthMethodOIDCPrefix) {
		return session.RenewFailed, "not an OIDC session"
	}
	targets, ok := r.Store.(SessionTargets)
	if !ok {
		return session.RenewFailed, "the store cannot renew sessions"
	}
	providerID := strings.TrimPrefix(s.AuthMethod, session.AuthMethodOIDCPrefix)
	cfgs, err := r.Configs.OIDCProviders(ctx)
	if err != nil {
		return session.RenewUnavailable, "load providers"
	}
	var cfg Config
	found := false
	for _, c := range cfgs {
		if c.ID == providerID && !c.Disabled {
			cfg, found = c, true
			break
		}
	}
	if !found {
		return session.RenewFailed, "provider unknown or disabled"
	}
	// Two attempts: a background refresh or a sign-in may change the stored
	// token between the read and the write, and then the update is dropped;
	// the second attempt uses the token that is stored now.
	for range 2 {
		t, ok, err := targets.RefreshTargetOf(ctx, s.UserID, providerID)
		if err != nil {
			return session.RenewUnavailable, "read the refresh token"
		}
		if !ok {
			return session.RenewFailed, "no refresh token stored"
		}
		if t.Issuer != cfg.Issuer {
			return session.RenewFailed, "the provider has another issuer now"
		}
		p, err := r.provider(ctx, cfg)
		if err != nil {
			return session.RenewUnavailable, "discovery"
		}
		res, err := p.Refresh(ctx, t.RefreshToken, Identity{Issuer: t.Issuer, Subject: t.Subject})
		switch {
		case errors.Is(err, ErrRefreshRejected):
			out, aerr := r.Store.Apply(ctx, Update{Target: t, Suspend: true, OnApplied: r.auditSuspend})
			if aerr == nil && !out.Applied {
				continue // the token changed meanwhile: ask again with the new one
			}
			return session.RenewFailed, "refresh token rejected"
		case err != nil:
			if res.RotatedToken != "" {
				if out, aerr := r.Store.Apply(ctx, Update{Target: t, NewToken: res.RotatedToken}); aerr != nil || !out.Applied {
					r.logger().Warn("oidc: session renewal: rotated token not stored", "account", t)
				}
			}
			if errors.Is(err, ErrNoFreshClaims) || errors.Is(err, ErrIdentityMismatch) {
				return session.RenewFailed, "no usable fresh claims"
			}
			return session.RenewUnavailable, "refresh failed"
		}
		roles := cfg.Rules().Evaluate(res.Identity.Claims)
		out, err := r.Store.Apply(ctx, Update{Target: t, ReplaceRoles: true, Roles: roles, NewToken: res.RotatedToken, OnApplied: r.auditRoleChange})
		if err != nil {
			return session.RenewUnavailable, "store the result"
		}
		if !out.Applied {
			continue
		}
		if len(out.RolesAfter) == 0 {
			return session.RenewFailed, "no role left"
		}
		return session.RenewOK, ""
	}
	return session.RenewUnavailable, "the account changed during the renewal"
}
