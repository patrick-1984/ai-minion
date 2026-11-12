package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/oidc"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// oidcModuleOrder places the OIDC module after the auth module (30), whose
// session manager and rate limiter it uses, and before the web module (80),
// which reads the login providers and banners it registers.
const oidcModuleOrder = 35

// insecureBannerTTL is how long the admin banner trusts its last look at the
// providers; the banner runs on every page render.
const insecureBannerTTL = 15 * time.Second

func init() {
	// The OIDC settings are read on every sign-in, so a panel change applies
	// at once. The CA file path is read through the same resolver.
	for _, key := range []string{
		config.KeyOIDCName, config.KeyOIDCIssuer, config.KeyOIDCClientID, config.KeyOIDCClientSecret,
		config.KeyOIDCRoleClaim, config.KeyOIDCAdminValues, config.KeyOIDCUserValues,
		config.KeyOIDCScopes, config.KeyOIDCTimeout, config.KeyOIDCTLSInsecure,
	} {
		DeclareLiveSetting(key)
	}
	registerOIDC(DefaultRegistry, web.DefaultRegistry)
}

// registerOIDC registers the OIDC module in r (Story 6.2). The module
//
//   - serves GET /auth/oidc/{provider}/login and GET /auth/oidc/callback;
//   - registers one "Sign in with <name>" login provider per provider that
//     is enabled at start in wr (the page lists a provider added later after
//     the next restart; its sign-in works at once);
//   - registers the admin banner that warns permanently while a provider
//     skips TLS verification (R96, UX-DR8) and logs the same warning at start;
//   - provides the operation handlers that the REST framework binds.
func registerOIDC(r *Registry, wr *web.Registry) {
	err := r.Register("oidc", oidcModuleOrder, func(ctx context.Context, env *Env) error {
		return initOIDC(ctx, env, wr)
	}, DeclaresService[*oidc.Service](), DeclaresService[*api.OIDCHandlers]())
	if err != nil {
		panic(err)
	}
	if err := wr.RegisterBanner(insecureBanner); err != nil {
		panic(err)
	}
}

// providerSource is the oidc.ConfigSource of the Hub: the provider of the
// environment and the settings first, then the providers of the panel. It
// resolves on every call, so no change needs a restart.
type providerSource struct {
	resolver *config.Resolver
	store    interface {
		ListOIDCProviders(ctx context.Context) ([]store.OIDCProviderRow, error)
		ListRoleRules(ctx context.Context, providerID string) ([]store.RoleRuleRow, error)
	}
	log *slog.Logger
}

// settings returns the provider of the settings, and false when none is set.
func (p *providerSource) settings() (oidc.Config, bool, config.HubOIDC, error) {
	h, err := config.ResolveHubOIDC(p.resolver)
	if err != nil {
		return oidc.Config{}, false, config.HubOIDC{}, err
	}
	cfg, ok := oidc.FromSettings(h)
	return cfg, ok, h, nil
}

// OIDCProviders implements oidc.ConfigSource. A settings error (for example
// an issuer without a client id) hides that one provider and is logged; the
// providers of the panel still work.
func (p *providerSource) OIDCProviders(ctx context.Context) ([]oidc.Config, error) {
	var out []oidc.Config
	timeout := config.DefaultOIDCTimeout
	cfg, ok, h, err := p.settings()
	switch {
	case err != nil:
		p.log.Error("OIDC settings are invalid; the provider of the settings is off", "error", err)
	default:
		timeout = h.Timeout
		if ok {
			out = append(out, cfg)
		}
	}
	rows, err := p.store.ListOIDCProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list OIDC providers: %w", err)
	}
	if len(rows) == 0 {
		return out, nil
	}
	rules, err := p.store.ListRoleRules(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list role rules: %w", err)
	}
	for _, row := range rows {
		out = append(out, oidc.FromRow(row, rules, timeout))
	}
	return out, nil
}

// insecureNames returns the names of the enabled providers that skip TLS
// verification.
func insecureNames(cfgs []oidc.Config) []string {
	var out []string
	for _, c := range cfgs {
		if c.InsecureSkipVerify && !c.Disabled {
			out = append(out, c.Name)
		}
	}
	return out
}

func initOIDC(ctx context.Context, env *Env, wr *web.Registry) error {
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
	nw, err := Require[*Network](env)
	if err != nil {
		return err
	}
	mux, err := Require[*http.ServeMux](env)
	if err != nil {
		return err
	}
	mgr, err := Require[*session.Manager](env)
	if err != nil {
		return err
	}
	limiter, err := Require[*ratelimit.Limiter](env)
	if err != nil {
		return err
	}
	log := cfg.logger()
	resolver := settings.Resolver()
	src := &providerSource{resolver: resolver, store: st, log: log}

	// An invalid environment value stops the start, as for session_ttl. A bad
	// panel value must never lock the administrator out of the panel that can
	// fix it: the source logs it and hides that provider.
	//
	// The environment alone is checked value by value. The rule "an issuer
	// needs a client id" is checked against the environment plus the panel,
	// because the client id may come from the panel (AD-19 precedence).
	envOnly := *resolver
	envOnly.Panel = nil
	if _, err := config.ResolveHubOIDC(&envOnly); err != nil {
		if !errors.Is(err, config.ErrOIDCClientIDMissing) {
			return fmt.Errorf("OIDC settings: %w", err)
		}
		if _, merr := config.ResolveHubOIDC(resolver); errors.Is(merr, config.ErrOIDCClientIDMissing) {
			return fmt.Errorf("OIDC settings: %w", err)
		}
	}
	startCfgs, err := src.OIDCProviders(ctx)
	if err != nil {
		return err
	}
	for _, c := range startCfgs {
		if c.Disabled {
			continue
		}
		if err := c.Validate(); err != nil {
			log.Error("OIDC provider is not usable", "provider", c.ID, "error", err)
			continue
		}
		if c.InsecureSkipVerify {
			log.Warn("TLS verification of the OIDC provider is DISABLED; anyone on the network path can impersonate the identity provider. Use a CA file (MINION_OIDC_CA_FILE) instead",
				"provider", c.ID, "issuer", c.Issuer)
		}
		if err := wr.RegisterLoginProvider(web.LoginProvider{ID: c.ID, Label: c.Name, Path: oidc.LoginPath(c.ID), Order: 100}); err != nil {
			log.Warn("OIDC login button not registered", "provider", c.ID, "error", err)
		}
	}

	svc := &oidc.Service{
		Configs: src, Store: st, Sessions: mgr, Limiter: limiter, Audit: alog, Log: log,
		BaseURL: nw.BaseURL,
		TTL: func(context.Context) (time.Duration, error) {
			ttl, err := config.ResolveSessionTTL(resolver)
			if err != nil {
				return 0, err
			}
			return ttl.Value, nil
		},
		Secure:   func(*http.Request) bool { return session.CookieSecure(nw.PublicURL()) },
		ClientIP: nw.ClientIP,
	}
	svc.Register(mux)
	mw, err := Require[*session.Middleware](env)
	if err != nil {
		return err
	}
	var trusted []string
	if o := originOf(nw.PublicURL()); o != "" {
		trusted = append(trusted, o)
	}
	if err := svc.RegisterReauth(mux, mw, trusted...); err != nil {
		return err
	}

	handlers := &api.OIDCHandlers{
		Store: st, Audit: alog,
		Settings: func(context.Context) (oidc.Config, bool, error) {
			c, ok, _, err := src.settings()
			return c, ok, err
		},
	}
	if err := Provide(env, svc); err != nil {
		return err
	}
	if err := Provide(env, handlers); err != nil {
		return err
	}

	b := &bannerState{src: src}
	oidcBannerSource.Store(b)
	env.OnStop(func(context.Context) error {
		oidcBannerSource.CompareAndSwap(b, nil)
		return nil
	})
	return nil
}

// oidcBannerSource is the running Hub's view for the banner. The banner
// function is registered once per web registry, while a Hub (in a test, several
// in one process) comes and goes.
var oidcBannerSource atomic.Pointer[bannerState]

type bannerState struct {
	src *providerSource

	mu      sync.Mutex
	checked time.Time
	names   []string
}

// insecureProviders returns the names of the providers that skip TLS
// verification, from a look that is at most insecureBannerTTL old.
func (b *bannerState) insecureProviders(ctx context.Context) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.checked.IsZero() && time.Since(b.checked) < insecureBannerTTL {
		return b.names
	}
	cfgs, err := b.src.OIDCProviders(ctx)
	if err != nil {
		return b.names // keep the last answer; the error is not the page's business
	}
	b.names, b.checked = insecureNames(cfgs), time.Now()
	return b.names
}

// insecureBanner is the permanent warning for admins while any provider skips
// TLS verification (R96, UX-DR8).
func insecureBanner(r *http.Request) []web.Banner {
	sess, ok := session.FromContext(r.Context())
	b := oidcBannerSource.Load()
	if !ok || sess.Role != authz.RoleAdmin || b == nil {
		return nil
	}
	names := b.insecureProviders(r.Context())
	if len(names) == 0 {
		return nil
	}
	text := "Warning: TLS verification is turned off for the OIDC provider"
	if len(names) > 1 {
		text += "s"
	}
	for i, n := range names {
		if i == 0 {
			text += " "
		} else {
			text += ", "
		}
		text += `"` + n + `"`
	}
	text += ". Anyone on the network path can impersonate the identity provider and sign in as any user. Install its CA file (MINION_OIDC_CA_FILE) and turn the insecure flag off."
	return []web.Banner{{Kind: "danger", Text: text}}
}
