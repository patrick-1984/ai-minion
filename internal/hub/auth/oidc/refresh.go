package oidc

// Background role refresh (AD-10, Story 6.3).
//
// A Hub re-checks the IdP roles of every OIDC account in the background, so
// that removing someone from a group at the IdP removes their IdP role at the
// Hub without waiting for the next sign-in. The rules are:
//
//   - A refresh counts only if it returns fresh claims: the IdP must answer
//     the refresh-token grant with an ID token that verifies and names the
//     stored (issuer, subject). Only then are the roles re-evaluated with the
//     role rules that are configured now (not the ones the cached provider was
//     discovered with), and the source-idp roles replaced.
//   - An IdP that cannot be reached (network error, timeout, 5xx, a token
//     endpoint answer that is not a clear rejection, a failed discovery)
//     changes nothing.
//   - A refresh token that the IdP permanently rejects (invalid_grant)
//     suspends the IdP roles of that account until its next OIDC login: the
//     source-idp roles are removed, the stored token is dropped (so nothing is
//     retried), and the browser sessions end, because an account without any
//     IdP role must not keep an access that came from the IdP. The next OIDC
//     login stores a new token and re-evaluates the roles, which restores
//     them.
//   - A refresh never overwrites a later sign-in: its changes apply only while
//     the stored token and the sign-in revision of the identity are the ones
//     it started with.
//   - Local grants and Worker pairings are never touched (R94): the only
//     tables written are the source-idp rows of user_roles, the refresh token
//     of external_identities and, when the IdP roles are removed, the
//     sessions of that account.
//
// The parts are small ports: [Refresher] (the schedule and the per-account
// decision) uses [RefreshStore] for persistence, [ConfigSource] for the
// providers, [Clock] for time and [Provider.Refresh] for the one IdP call.
// [SQLStore] is the adapter on the Hub database; it holds no SQL (AD-16): the
// queries and the transaction are [store.Store.ListOIDCRefreshTargets] and
// [store.Store.ApplyOIDCRefresh].

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

// SettingRefreshInterval is the settings key of the refresh interval, a Go
// duration such as "24h". The Hub declares it in the settings registry (AD-19:
// MINION_OIDC_REFRESH_INTERVAL, then the panel, then the default).
const SettingRefreshInterval = config.KeyOIDCRefreshInterval

// Bounds of the refresh schedule.
const (
	// DefaultRefreshInterval is the interval when the setting is not set (AD-10).
	DefaultRefreshInterval = config.DefaultOIDCRefreshInterval
	// MinRefreshInterval and MaxRefreshInterval bound the setting. The lower
	// bound keeps a typo from hammering the IdP.
	MinRefreshInterval = config.MinOIDCRefreshInterval
	MaxRefreshInterval = config.MaxOIDCRefreshInterval
	// DefaultRefreshStartDelay is how long after the Hub started the first
	// refresh runs. A Hub that restarts more often than the interval would
	// otherwise never refresh.
	DefaultRefreshStartDelay = time.Minute
)

// Audit vocabulary of the background refresh (AD-20). The role change itself
// reuses [AuditActionRoleSync] with [OutcomeChanged]; the actor is "system".
const (
	// AuditActionRoleSuspend records that a rejected refresh token suspended
	// the IdP roles of an account.
	AuditActionRoleSuspend = "role.suspend"

	systemActor = "system"
)

// ParseRefreshInterval parses the value of [SettingRefreshInterval]. Spaces
// around the value are ignored. An empty value is the default; anything else is
// a Go duration between [MinRefreshInterval] and [MaxRefreshInterval].
func ParseRefreshInterval(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultRefreshInterval, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("oidc: %s must be a duration such as 24h: %w", SettingRefreshInterval, err)
	}
	if d < MinRefreshInterval || d > MaxRefreshInterval {
		return 0, fmt.Errorf("oidc: %s must be between %s and %s", SettingRefreshInterval, MinRefreshInterval, MaxRefreshInterval)
	}
	return d, nil
}

// IntervalFromResolver returns the interval source of a [Refresher]. It
// resolves [SettingRefreshInterval] through r on every call (environment, then
// the panel, then the default: AD-19), so a panel change applies without a
// restart, and parses and bounds the value with [ParseRefreshInterval]. An
// error means the value is not usable; the Refresher then logs a warning and
// uses [DefaultRefreshInterval].
func IntervalFromResolver(r *config.Resolver) func(ctx context.Context) (time.Duration, error) {
	return func(context.Context) (time.Duration, error) {
		res, err := r.Resolve(SettingRefreshInterval)
		if err != nil {
			return 0, err
		}
		d, err := ParseRefreshInterval(res.Value)
		if err != nil {
			return 0, fmt.Errorf("%w (from %s)", err, res.Source)
		}
		return d, nil
	}
}

// ---- the IdP call -------------------------------------------------------------

// Errors of a refresh, matched with [errors.Is].
var (
	// ErrRefreshRejected means the IdP permanently rejected the refresh token
	// (invalid_grant: revoked, expired or already used).
	ErrRefreshRejected = errors.New("oidc: the refresh token was rejected by the identity provider")
	// ErrNoFreshClaims means the grant succeeded but returned no ID token, so
	// there are no fresh claims to evaluate.
	ErrNoFreshClaims = errors.New("oidc: the refresh returned no ID token")
	// ErrIdentityMismatch means the refreshed ID token names another subject
	// than the account the token was stored for.
	ErrIdentityMismatch = errors.New("oidc: the refreshed ID token names another identity")
)

// RefreshResult is what the refresh-token grant returned.
type RefreshResult struct {
	// Identity is the verified identity with its fresh claims. It is the zero
	// value unless the refresh counts (the error is nil).
	Identity Identity
	// RotatedToken is a new refresh token when the IdP issued one that differs
	// from the one sent; it must replace the stored token even when the claims
	// are not usable, because the old token may be dead now.
	RotatedToken logx.Secret
}

// Refresh runs the refresh-token grant under the provider's deadline and
// verifies the ID token it returns (signature, issuer, audience, expiry; a
// refresh response need not carry the sign-in nonce). The identity must be
// want: the same issuer and subject the token was stored for.
//
// The error is nil only for fresh claims. [ErrRefreshRejected] is a permanent
// rejection; every other error (network, timeout, 5xx, unusable answer) means
// "nothing learned" and the caller changes no role. The text of an IdP answer
// is never part of an error.
func (p *Provider) Refresh(ctx context.Context, refreshToken logx.Secret, want Identity) (RefreshResult, error) {
	ctx, cancel := context.WithTimeout(gooidc.ClientContext(ctx, p.client), p.cfg.timeout())
	defer cancel()
	// A token without an access token is expired for the oauth2 package, so
	// the source goes straight to the token endpoint.
	src := p.oauth("").TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken.Reveal(), Expiry: time.Unix(1, 0)})
	tok, err := src.Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.ErrorCode == "invalid_grant" &&
			(re.Response.StatusCode == 400 || re.Response.StatusCode == 401) {
			return RefreshResult{}, ErrRefreshRejected
		}
		return RefreshResult{}, fmt.Errorf("oidc: refresh failed: %w", sanitizeExchangeError(err))
	}
	var res RefreshResult
	if tok.RefreshToken != "" && tok.RefreshToken != refreshToken.Reveal() {
		res.RotatedToken = logx.Secret(tok.RefreshToken)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return res, ErrNoFreshClaims
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrInvalidIDToken, err)
	}
	if idt.Issuer != want.Issuer || idt.Subject != want.Subject {
		return res, ErrIdentityMismatch
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return res, fmt.Errorf("%w: claims: %w", ErrInvalidIDToken, err)
	}
	res.Identity = Identity{Issuer: idt.Issuer, Subject: idt.Subject, Claims: claims}
	return res, nil
}

// ---- persistence port -----------------------------------------------------------

// Target is one account to refresh: an external identity with a stored
// refresh token, of an active user.
type Target struct {
	IdentityID string
	UserID     string
	Issuer     string
	Subject    string
	// ProviderID is the provider the account last signed in with.
	ProviderID string
	// LastLoginAt is the sign-in revision of the identity (unix milliseconds).
	// It advances at every sign-in, also when the sign-in keeps the token.
	LastLoginAt int64
	// RefreshToken is the stored token, decrypted. It is a secret.
	RefreshToken logx.Secret
}

// LogValue logs the ids, never the token.
func (t Target) LogValue() slog.Value {
	return slog.GroupValue(slog.String("identity", t.IdentityID), slog.String("user", t.UserID), slog.String("provider", t.ProviderID))
}

// Update is one change of the store, applied only while the identity is still
// as Target describes it: the stored token is Target.RefreshToken and the
// sign-in revision is Target.LastLoginAt. A sign-in in the meantime, with a new,
// the same or no refresh token, therefore wins over a refresh that started
// before it.
type Update struct {
	Target Target
	// ReplaceRoles replaces the source-idp roles with Roles (which may be
	// empty). It is false for an update that only stores a rotated token.
	ReplaceRoles bool
	Roles        []string
	// NewToken, when not empty, replaces the stored refresh token.
	NewToken logx.Secret
	// Suspend removes the source-idp roles and the stored token (and ignores
	// ReplaceRoles, Roles and NewToken).
	Suspend bool
	// OnApplied, when not nil, runs inside the transaction after the change,
	// so the audit rows commit with the change they describe. An error rolls
	// the update back.
	OnApplied func(ctx context.Context, ex store.Execer, res UpdateResult) error
}

// UpdateResult is the outcome of [RefreshStore.Apply].
type UpdateResult struct {
	// Applied is false when nothing was changed: the identity is gone, the user
	// was deactivated, or the identity changed (another token, or a newer
	// sign-in) since the refresh started.
	Applied bool
	UserID  string
	// RolesBefore and RolesAfter are the source-idp roles, sorted.
	RolesBefore, RolesAfter []string
	// SessionsEnded is the number of browser sessions deleted because the
	// account lost its last IdP role.
	SessionsEnded int64
}

// RolesChanged reports whether the source-idp roles changed.
func (r UpdateResult) RolesChanged() bool { return !slices.Equal(r.RolesBefore, r.RolesAfter) }

// RefreshStore is the persistence port of the [Refresher]; [SQLStore]
// implements it.
type RefreshStore interface {
	// RefreshTargets lists the accounts that hold a refresh token, active
	// users only.
	RefreshTargets(ctx context.Context) ([]Target, error)
	// Apply applies one update atomically (see [Update]).
	Apply(ctx context.Context, u Update) (UpdateResult, error)
}

var _ RefreshStore = (*SQLStore)(nil)

// ---- the refresher ----------------------------------------------------------------

// Clock is the time source of the schedule; tests replace it.
type Clock interface {
	// After returns a channel that receives once, after d.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RunSummary counts what one pass did.
type RunSummary struct {
	// Targets is the number of accounts with a refresh token.
	Targets int
	// Refreshed counts accounts whose refresh returned fresh claims and was
	// applied; Changed is the subset whose IdP roles changed.
	Refreshed, Changed int
	// Suspended counts accounts whose IdP roles were suspended.
	Suspended int
	// Unchanged counts accounts left as they were because the IdP could not be
	// reached or did not return usable fresh claims.
	Unchanged int
	// Skipped counts accounts whose provider is unknown, disabled or has
	// another issuer now.
	Skipped int
}

// Refresher re-checks the IdP roles of every account in the background.
type Refresher struct {
	Configs ConfigSource
	Store   RefreshStore
	Audit   Auditor
	Log     *slog.Logger

	// Interval returns the refresh interval; it is called after every pass. A
	// nil Interval, an error or a value outside the bounds means
	// [DefaultRefreshInterval] (or the nearest bound). Use
	// [IntervalFromResolver] for the oidc_refresh_interval setting.
	Interval func(ctx context.Context) (time.Duration, error)
	// StartDelay is the wait before the first pass; 0 means
	// [DefaultRefreshStartDelay].
	StartDelay time.Duration
	// Clock defaults to the system clock.
	Clock Clock
	// Discover defaults to [Discover]; tests replace it.
	Discover func(ctx context.Context, cfg Config) (*Provider, error)

	mu    sync.Mutex
	cache map[string]cachedProvider
}

func (r *Refresher) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Refresher) clock() Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return realClock{}
}

// interval returns the delay to the next pass, always within the bounds.
func (r *Refresher) interval(ctx context.Context) time.Duration {
	if r.Interval == nil {
		return DefaultRefreshInterval
	}
	d, err := r.Interval(ctx)
	if err != nil {
		r.logger().Warn("oidc refresh: the interval setting is not usable, using the default", "error", err)
		return DefaultRefreshInterval
	}
	return min(max(d, MinRefreshInterval), MaxRefreshInterval)
}

// Run refreshes after the start delay and then once per interval until ctx
// ends. A failing pass is logged and never stops the loop. It returns
// ctx.Err().
func (r *Refresher) Run(ctx context.Context) error {
	delay := r.StartDelay
	if delay <= 0 {
		delay = DefaultRefreshStartDelay
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.clock().After(delay):
		}
		sum, err := r.RunOnce(ctx)
		if err != nil {
			r.logger().Error("oidc refresh: pass failed", "error", err)
		} else {
			r.logger().Info("oidc refresh: pass done", "targets", sum.Targets, "refreshed", sum.Refreshed,
				"changed", sum.Changed, "suspended", sum.Suspended, "unchanged", sum.Unchanged, "skipped", sum.Skipped)
		}
		delay = r.interval(ctx)
	}
}

// RunOnce refreshes every account that holds a refresh token, one after the
// other. An error is returned only when the accounts or the providers cannot
// be listed or ctx ended; a failure of one account is counted and logged, and
// the pass goes on.
func (r *Refresher) RunOnce(ctx context.Context) (RunSummary, error) {
	var sum RunSummary
	cfgs, err := r.Configs.OIDCProviders(ctx)
	if err != nil {
		return sum, fmt.Errorf("oidc refresh: load providers: %w", err)
	}
	targets, err := r.Store.RefreshTargets(ctx)
	if err != nil {
		return sum, fmt.Errorf("oidc refresh: list accounts: %w", err)
	}
	sum.Targets = len(targets)
	byID := make(map[string]Config, len(cfgs))
	for _, c := range cfgs {
		if !c.Disabled {
			byID[c.ID] = c
		}
	}
	// One discovery attempt per provider and pass: an IdP that is down is not
	// asked once per account.
	type discovered struct {
		p   *Provider
		err error
	}
	seen := map[string]discovered{}
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		cfg, ok := byID[t.ProviderID]
		if !ok || cfg.Issuer != t.Issuer {
			sum.Skipped++
			continue
		}
		d, ok := seen[cfg.ID]
		if !ok {
			d.p, d.err = r.provider(ctx, cfg)
			seen[cfg.ID] = d
		}
		if d.err != nil {
			r.logger().Warn("oidc refresh: identity provider not reachable, nothing changed", "provider", cfg.ID, "error", d.err)
			sum.Unchanged++
			continue
		}
		r.refreshOne(ctx, d.p, cfg, t, &sum)
	}
	return sum, nil
}

// provider returns the discovered provider for cfg, discovering it again when
// the configuration changed or no earlier discovery succeeded.
func (r *Refresher) provider(ctx context.Context, cfg Config) (*Provider, error) {
	fp := cfg.fingerprint()
	r.mu.Lock()
	c, ok := r.cache[cfg.ID]
	r.mu.Unlock()
	if ok && c.fingerprint == fp {
		return c.provider, nil
	}
	discover := r.Discover
	if discover == nil {
		discover = Discover
	}
	p, err := discover(ctx, cfg)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedProvider{}
	}
	r.cache[cfg.ID] = cachedProvider{fingerprint: fp, provider: p}
	r.mu.Unlock()
	return p, nil
}

// refreshOne refreshes one account and applies the decision of the rules above.
// cfg is the configuration of this pass: its role rules decide. p is the
// cached, discovered provider and serves only the network call; its own
// configuration is as old as the last discovery and its fingerprint leaves the
// role rules out, so it must not decide the roles.
func (r *Refresher) refreshOne(ctx context.Context, p *Provider, cfg Config, t Target, sum *RunSummary) {
	res, err := p.Refresh(ctx, t.RefreshToken, Identity{Issuer: t.Issuer, Subject: t.Subject})
	switch {
	case errors.Is(err, ErrRefreshRejected):
		r.apply(ctx, t, Update{Target: t, Suspend: true, OnApplied: r.auditSuspend}, sum, true)
		return
	case err != nil:
		// Nothing was learned. A rotated token must still be kept: the IdP
		// may have invalidated the one that is stored.
		r.logger().Warn("oidc refresh: no fresh claims, roles unchanged", "account", t, "error", err)
		if res.RotatedToken != "" {
			r.apply(ctx, t, Update{Target: t, NewToken: res.RotatedToken}, sum, false)
		}
		sum.Unchanged++
		return
	}
	roles := cfg.Rules().Evaluate(res.Identity.Claims)
	r.apply(ctx, t, Update{Target: t, ReplaceRoles: true, Roles: roles, NewToken: res.RotatedToken, OnApplied: r.auditRoleChange}, sum, false)
}

// apply runs u and updates the summary. suspend marks a suspension.
func (r *Refresher) apply(ctx context.Context, t Target, u Update, sum *RunSummary, suspend bool) {
	out, err := r.Store.Apply(ctx, u)
	switch {
	case err != nil:
		r.logger().Error("oidc refresh: could not store the result, roles unchanged", "account", t, "error", err)
		sum.Unchanged++
	case !out.Applied:
		r.logger().Info("oidc refresh: the account changed meanwhile, result dropped", "account", t)
		sum.Unchanged++
	case suspend:
		r.logger().Warn("oidc refresh: refresh token rejected, IdP roles suspended until the next OIDC login", "account", t)
		sum.Suspended++
	case u.ReplaceRoles:
		sum.Refreshed++
		if out.RolesChanged() {
			sum.Changed++
		}
	}
}

func (r *Refresher) auditRoleChange(ctx context.Context, ex store.Execer, res UpdateResult) error {
	if !res.RolesChanged() {
		return nil
	}
	return r.Audit.RecordTx(ctx, ex, audit.Event{Actor: systemActor, Action: AuditActionRoleSync, Target: res.UserID, Outcome: OutcomeChanged})
}

func (r *Refresher) auditSuspend(ctx context.Context, ex store.Execer, res UpdateResult) error {
	return r.Audit.RecordTx(ctx, ex, audit.Event{Actor: systemActor, Action: AuditActionRoleSuspend, Target: res.UserID, Outcome: OutcomeSuccess})
}

// ---- the database adapter -----------------------------------------------------------

// SQLStore is the [RefreshStore] on the Hub database: a thin adapter on
// [store.Store.ListOIDCRefreshTargets] and [store.Store.ApplyOIDCRefresh],
// which hold the SQL (AD-16).
type SQLStore struct {
	Store *store.Store
	// Log is optional; it reports a stored token that cannot be opened.
	Log *slog.Logger
	// Now defaults to [time.Now]; tests fix it.
	Now func() time.Time
}

func (s *SQLStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// RefreshTargets implements [RefreshStore]. A token that cannot be opened (a
// damaged row) is skipped and logged without its value.
func (s *SQLStore) RefreshTargets(ctx context.Context) ([]Target, error) {
	rows, unreadable, err := s.Store.ListOIDCRefreshTargets(ctx)
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	if s.Log != nil {
		for _, id := range unreadable {
			s.Log.Error("oidc refresh: a stored refresh token cannot be opened, skipped", "identity", id)
		}
	}
	out := make([]Target, 0, len(rows))
	for _, x := range rows {
		out = append(out, Target{
			IdentityID: x.IdentityID, UserID: x.UserID, Issuer: x.Issuer, Subject: x.Subject, ProviderID: x.ProviderID,
			LastLoginAt: x.LastLoginAt, RefreshToken: logx.Secret(x.RefreshToken),
		})
	}
	return out, nil
}

// Apply implements [RefreshStore].
func (s *SQLStore) Apply(ctx context.Context, u Update) (UpdateResult, error) {
	su := store.OIDCRefreshUpdate{
		IdentityID: u.Target.IdentityID, UserID: u.Target.UserID,
		ExpectedToken: u.Target.RefreshToken.Reveal(), ExpectedLastLoginAt: u.Target.LastLoginAt,
		ReplaceRoles: u.ReplaceRoles, Roles: u.Roles, NewToken: u.NewToken.Reveal(), Suspend: u.Suspend,
		NowMS: store.UnixMS(s.now()),
	}
	if u.OnApplied != nil {
		su.OnApplied = func(ctx context.Context, ex store.Execer, res store.OIDCRefreshResult) error {
			return u.OnApplied(ctx, ex, updateResult(res))
		}
	}
	res, err := s.Store.ApplyOIDCRefresh(ctx, su)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("oidc: %w", err)
	}
	return updateResult(res), nil
}

func updateResult(r store.OIDCRefreshResult) UpdateResult {
	return UpdateResult{Applied: r.Applied, UserID: r.UserID, RolesBefore: r.RolesBefore, RolesAfter: r.RolesAfter, SessionsEnded: r.SessionsEnded}
}
