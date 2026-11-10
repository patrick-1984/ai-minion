package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// authModuleOrder places the auth module after the store (10), authz (15),
// settings and audit (20) modules it requires.
const authModuleOrder = 30

func init() {
	// session_ttl is read on every login, so a panel change applies at once.
	DeclareLiveSetting(config.KeySessionTTL)
	registerAuth(DefaultRegistry)
}

// registerAuth registers the auth module in r (Story 3.5). It
//
//   - seeds the local admin once, on an empty user table;
//   - wires the user lookup of the authorizer;
//   - provides the one shared *ratelimit.Limiter of the Hub (AD-19), the
//     session manager and the session middleware that the REST framework and
//     the web UI use;
//   - serves POST /auth/login and POST /auth/logout.
func registerAuth(r *Registry) {
	err := r.Register("auth", authModuleOrder, initAuth,
		DeclaresService[*ratelimit.Limiter](),
		DeclaresService[*session.Manager](),
		DeclaresService[*session.Middleware](),
	)
	if err != nil {
		panic(err)
	}
}

func initAuth(ctx context.Context, env *Env) error {
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
	nw, err := Require[*Network](env)
	if err != nil {
		return err
	}
	mux, err := Require[*http.ServeMux](env)
	if err != nil {
		return err
	}

	resolver := settings.Resolver()
	// session_ttl is checked on every start: an invalid environment value
	// stops the start; an invalid panel value falls back to the default.
	ttl, err := config.ResolveSessionTTL(resolver)
	if err != nil {
		return fmt.Errorf("auth settings: %w", err)
	}
	if ttl.Fallback {
		cfg.logger().Error("setting has an invalid value; using the default",
			"key", config.KeySessionTTL, "source", string(ttl.Source))
	}

	hasher := local.NewHasher()
	// The seed settings are resolved only when the user table is empty. Once
	// the Hub has an account, the one-time bootstrap secret (for example
	// MINION_ADMIN_PASSWORD_FILE) is no longer needed and may be gone. The
	// Seeder repeats the check inside its transaction, so two starts at the
	// same time still create one admin.
	hasUsers, err := st.HasUsers(ctx)
	if err != nil {
		return fmt.Errorf("auth: check for accounts: %w", err)
	}
	if !hasUsers {
		ha, err := config.ResolveHubAuth(resolver)
		if err != nil {
			return fmt.Errorf("auth settings: %w", err)
		}
		seeder := &local.Seeder{Store: st, Hasher: hasher, Log: cfg.logger()}
		if _, err := seeder.Seed(ctx, local.SeedConfig{Username: ha.AdminUsername, Password: logx.Secret(ha.AdminPassword)}); err != nil {
			return err
		}
	}

	if err := az.SetUserLookup(userLookup{st: st}); err != nil {
		return fmt.Errorf("wire the user lookup: %w", err)
	}

	limiter, err := ratelimit.New(ratelimit.Config{})
	if err != nil {
		return fmt.Errorf("rate limiter: %w", err)
	}
	mgr := session.NewManager(st)
	mw := &session.Middleware{Manager: mgr, Log: cfg.logger()}
	for _, p := range []func() error{
		func() error { return Provide(env, limiter) },
		func() error { return Provide(env, mgr) },
		func() error { return Provide(env, mw) },
	} {
		if err := p(); err != nil {
			return err
		}
	}

	h := &local.Handler{
		Auth:     &local.Authenticator{Store: st, Hasher: hasher, Limiter: limiter, Audit: alog, Log: cfg.logger()},
		Sessions: mgr,
		Audit:    alog,
		// A panel value can change at any time and may be invalid: the login
		// then uses the default lifetime and logs the key and its source.
		TTL: func(context.Context) (time.Duration, error) {
			ttl, err := config.ResolveSessionTTL(resolver)
			if err != nil {
				return 0, err
			}
			if ttl.Fallback {
				cfg.logger().Error("setting has an invalid value; using the default",
					"key", config.KeySessionTTL, "source", string(ttl.Source))
			}
			return ttl.Value, nil
		},
		Secure:   func(*http.Request) bool { return session.CookieSecure(nw.PublicURL()) },
		ClientIP: func(r *http.Request) netip.Addr { return nw.ClientIP(r) },
		Log:      cfg.logger(),
	}
	// The public URL is a trusted origin, so that a proxy that rewrites the
	// Host header does not make the login look cross-origin.
	var trusted []string
	if o := originOf(nw.PublicURL()); o != "" {
		trusted = append(trusted, o)
	}
	if err := h.Register(mux, mw, trusted...); err != nil {
		return fmt.Errorf("register the auth endpoints: %w", err)
	}
	return nil
}

// originOf returns "scheme://host[:port]" of rawURL, or "" when it has none.
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// userLookup adapts the users table to the authorizer's user port. It reads
// current state on every call (no cache), as authz requires.
type userLookup struct{ st *store.Store }

// User implements authz.UserLookup.
func (u userLookup) User(ctx context.Context, userID string) (authz.UserState, error) {
	row, err := u.st.UserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return authz.UserState{}, authz.ErrNotFound
		}
		return authz.UserState{}, err
	}
	return authz.UserState{Role: authz.Role(row.Role), Active: row.Active}, nil
}
