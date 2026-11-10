package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/oauth2"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// Paths of the sign-in endpoints. The redirect URI registered at the IdP is
// the public URL plus [CallbackPath].
const (
	// LoginPathFormat is the path that starts a sign-in; %s is the provider id.
	LoginPathFormat = "/auth/oidc/%s/login"
	// CallbackPath is the redirect URI path, shared by every provider.
	CallbackPath = "/auth/oidc/callback"
	// BindingCookie is the cookie that ties a callback to the browser that
	// started the sign-in.
	BindingCookie = "minion_oidc"
)

// LoginPath returns the path that starts a sign-in with the provider.
func LoginPath(providerID string) string { return fmt.Sprintf(LoginPathFormat, providerID) }

// Audit vocabulary of OIDC sign-ins (AD-20). "login" is shared with the
// password login; no event carries a token, a code or a claim value.
const (
	AuditActionLogin    = "login"
	AuditActionRoleSync = "role.sync"

	OutcomeSuccess     = "success"
	OutcomeFailed      = "failed"
	OutcomeDenied      = "denied"
	OutcomeInactive    = "inactive"
	OutcomeRateLimited = "rate_limited"
	OutcomeChanged     = "changed"

	anonymousActor = "anonymous"
)

// loginTxTTL is how long a started sign-in may wait for its callback.
const loginTxTTL = 10 * time.Minute

// Store is the persistence port of the [Service]; *store.Store implements it.
type Store interface {
	CreateOIDCLoginTx(ctx context.Context, t store.OIDCLoginTx, nowMS int64) error
	ConsumeOIDCLoginTx(ctx context.Context, stateHash string, nowMS int64) (store.OIDCLoginTx, error)
	SyncOIDCLogin(ctx context.Context, p store.OIDCSyncParams) (store.OIDCSyncResult, error)
}

var _ Store = (*store.Store)(nil)

// Sessions issues browser sessions; *session.Manager implements it.
type Sessions interface {
	Create(ctx context.Context, user store.UserRow, ttl time.Duration) (session.Issued, error)
	Revoke(ctx context.Context, token string) error
}

var _ Sessions = (*session.Manager)(nil)

// Limiter is the part of the shared rate limiter that sign-in uses.
type Limiter interface {
	Allow(policy ratelimit.Policy, ip netip.Addr, target string) (bool, time.Duration)
}

var _ Limiter = (*ratelimit.Limiter)(nil)

// Auditor records audit events; *audit.Log implements it.
type Auditor interface {
	Record(ctx context.Context, ev audit.Event) error
	// RecordTx writes the event inside the caller's open transaction.
	RecordTx(ctx context.Context, tx store.Execer, ev audit.Event) error
}

var _ Auditor = (*audit.Log)(nil)

// Service runs the OIDC sign-in: it starts the authorization-code flow with
// PKCE, state and nonce, handles the callback and turns the verified identity
// into a browser session with roles from the IdP's claims.
//
// A failure of any step ends the sign-in with an English error page and, for
// every failure after the callback was accepted, leaves the roles of every
// account as they were: roles change only in the one transaction of
// [store.Store.SyncOIDCLogin], after the ID token was verified.
type Service struct {
	Configs  ConfigSource
	Store    Store
	Sessions Sessions
	Limiter  Limiter
	Audit    Auditor
	Log      *slog.Logger
	// Linker, when set, links a first sign-in to the local account with the
	// same e-mail address when the rules of Story 6.4 allow it, and refuses
	// the sign-in when they do not. Nil means no linking: every unknown
	// identity creates its own account.
	Linker IdentityLinker

	// BaseURL returns the origin of the Hub for a request: the public URL when
	// it is set. The redirect URI is BaseURL plus [CallbackPath].
	BaseURL func(r *http.Request) string
	// TTL returns the session lifetime; it is called on every sign-in.
	TTL func(ctx context.Context) (time.Duration, error)
	// Secure decides the Secure attribute of the cookies for a request.
	Secure func(r *http.Request) bool
	// ClientIP returns the client address for the rate limiter.
	ClientIP func(r *http.Request) netip.Addr
	// Now defaults to [time.Now]; tests fix it.
	Now func() time.Time
	// Discover defaults to [Discover]; tests replace it.
	Discover func(ctx context.Context, cfg Config) (*Provider, error)

	mu    sync.Mutex
	cache map[string]cachedProvider
}

type cachedProvider struct {
	fingerprint string
	provider    *Provider
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Register adds the sign-in endpoints to mux: GET /auth/oidc/{provider}/login
// and GET /auth/oidc/callback. Neither is behind the session middleware (the
// caller has no session yet) and the callback cannot sit behind a same-origin
// check, because the IdP redirects the browser to it; the state and the
// binding cookie protect it instead.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/oidc/{provider}/login", s.Start)
	mux.HandleFunc("GET "+CallbackPath, s.Callback)
}

// provider returns the discovered provider for cfg, discovering it again when
// the configuration changed or no earlier discovery succeeded.
func (s *Service) provider(ctx context.Context, cfg Config) (*Provider, error) {
	fp := cfg.fingerprint()
	s.mu.Lock()
	c, ok := s.cache[cfg.ID]
	s.mu.Unlock()
	if ok && c.fingerprint == fp {
		return c.provider, nil
	}
	discover := s.Discover
	if discover == nil {
		discover = Discover
	}
	p, err := discover(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]cachedProvider{}
	}
	s.cache[cfg.ID] = cachedProvider{fingerprint: fp, provider: p}
	s.mu.Unlock()
	return p, nil
}

// find returns the enabled provider with the given id.
func (s *Service) find(ctx context.Context, id string) (Config, error) {
	cfgs, err := s.Configs.OIDCProviders(ctx)
	if err != nil {
		return Config{}, err
	}
	for _, c := range cfgs {
		if c.ID == id && !c.Disabled {
			return c, nil
		}
	}
	return Config{}, errUnknownProvider
}

var errUnknownProvider = errors.New("oidc: unknown or disabled provider")

// allow applies the shared "login" policy. The target is the client address
// itself, so that one noisy client cannot use up the limit of everybody else
// (a shared target would).
func (s *Service) allow(r *http.Request) (bool, time.Duration) {
	ip := s.ClientIP(r)
	return s.Limiter.Allow(ratelimit.PolicyLogin, ip, "oidc:"+ip.String())
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Service) redirectURL(r *http.Request) string {
	return strings.TrimSuffix(s.BaseURL(r), "/") + CallbackPath
}

// Start handles GET /auth/oidc/{provider}/login: it records the sign-in
// (state, nonce, PKCE verifier and the hash of a binding cookie) and sends the
// browser to the IdP. An IdP that cannot be reached ends in an error page;
// nothing else changes.
func (s *Service) Start(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider")
	ctx := r.Context()
	if ok, wait := s.allow(r); !ok {
		s.record(ctx, anonymousActor, "oidc:"+id, OutcomeRateLimited)
		w.Header().Set("Retry-After", fmt.Sprint(int(wait/time.Second)+1))
		s.errorPage(w, r, http.StatusTooManyRequests, "Too many sign-in attempts", "Wait a moment and try again.")
		return
	}
	cfg, err := s.find(ctx, id)
	switch {
	case errors.Is(err, errUnknownProvider):
		s.errorPage(w, r, http.StatusNotFound, "Unknown sign-in method", "This sign-in method does not exist or is turned off.")
		return
	case err != nil:
		s.fail(w, r, "load providers", "", err)
		return
	}
	prov, err := s.provider(ctx, cfg)
	if err != nil {
		s.record(ctx, anonymousActor, "oidc:"+id, OutcomeFailed)
		s.fail(w, r, "discovery", id, err)
		return
	}

	state, err1 := randomString(32)
	nonce, err2 := randomString(32)
	binding, err3 := randomString(32)
	if err := errors.Join(err1, err2, err3); err != nil {
		s.fail(w, r, "random values", id, err)
		return
	}
	verifier := oauth2.GenerateVerifier()
	now := s.now()
	err = s.Store.CreateOIDCLoginTx(ctx, store.OIDCLoginTx{
		StateHash: store.HashValue(state), BindingHash: store.HashValue(binding),
		ProviderID: id, Nonce: nonce, CodeVerifier: verifier,
		NextPath:  safeNext(r.URL.Query().Get("next")),
		CreatedAt: store.UnixMS(now), ExpiresAt: store.UnixMS(now.Add(loginTxTTL)),
	}, store.UnixMS(now))
	if err != nil {
		s.fail(w, r, "store the sign-in", id, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: BindingCookie, Value: binding, Path: "/auth/oidc",
		MaxAge: int(loginTxTTL / time.Second), HttpOnly: true,
		Secure: s.Secure(r), SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, prov.AuthCodeURL(s.redirectURL(r), state, nonce, verifier), http.StatusFound)
}

// Callback handles GET /auth/oidc/callback.
//
// The order of the checks is: rate limit; the IdP's own error; the state (one
// use, not expired, same browser); the code exchange; an ID token that is
// present and verifies, with the nonce of this sign-in; the roles from the one
// configured claim; the account and its roles in one transaction; no matching
// rule means no access; the session.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	// The binding cookie is single use whatever the outcome.
	http.SetCookie(w, &http.Cookie{Name: BindingCookie, Value: "", Path: "/auth/oidc", MaxAge: -1,
		HttpOnly: true, Secure: s.Secure(r), SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")

	if ok, wait := s.allow(r); !ok {
		s.record(ctx, anonymousActor, "oidc:callback", OutcomeRateLimited)
		w.Header().Set("Retry-After", fmt.Sprint(int(wait/time.Second)+1))
		s.errorPage(w, r, http.StatusTooManyRequests, "Too many sign-in attempts", "Wait a moment and try again.")
		return
	}
	state := q.Get("state")
	if state == "" {
		s.errorPage(w, r, http.StatusBadRequest, "Sign-in failed", "The sign-in response has no state. Start the sign-in again.")
		return
	}
	tx, err := s.Store.ConsumeOIDCLoginTx(ctx, store.HashValue(state), store.UnixMS(s.now()))
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.record(ctx, anonymousActor, "oidc:callback", OutcomeFailed)
		s.errorPage(w, r, http.StatusBadRequest, "Sign-in failed", "The sign-in is unknown, was already used or has expired. Start the sign-in again.")
		return
	case err != nil:
		s.fail(w, r, "read the sign-in", "", err)
		return
	}
	target := "oidc:" + tx.ProviderID
	c, err := r.Cookie(BindingCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(store.HashValue(c.Value)), []byte(tx.BindingHash)) != 1 {
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		s.errorPage(w, r, http.StatusBadRequest, "Sign-in failed", "The sign-in was started in another browser. Start it again in this one.")
		return
	}
	if e := q.Get("error"); e != "" {
		// The text of the IdP's error is not shown or logged: it is chosen by
		// the sender of the URL.
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		s.errorPage(w, r, http.StatusBadGateway, "Sign-in refused", "The identity provider did not sign you in. Try again, or ask your administrator.")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		s.errorPage(w, r, http.StatusBadRequest, "Sign-in failed", "The sign-in response has no authorization code.")
		return
	}

	cfg, err := s.find(ctx, tx.ProviderID)
	if err != nil {
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		s.errorPage(w, r, http.StatusNotFound, "Unknown sign-in method", "This sign-in method does not exist or is turned off.")
		return
	}
	prov, err := s.provider(ctx, cfg)
	if err != nil {
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		s.fail(w, r, "discovery", cfg.ID, err)
		return
	}
	tokens, err := prov.Exchange(ctx, s.redirectURL(r), code, tx.CodeVerifier)
	if err != nil {
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		if errors.Is(err, ErrNoIDToken) {
			s.errorPage(w, r, http.StatusBadGateway, "Sign-in failed", "The identity provider returned no ID token. Check that the client may request the openid scope.")
			return
		}
		s.fail(w, r, "code exchange", cfg.ID, err)
		return
	}
	ident, err := prov.VerifyIDToken(ctx, tokens.IDToken, tx.Nonce)
	if err != nil {
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		s.logger().Warn("oidc: ID token refused", "provider", cfg.ID, "error", err)
		s.errorPage(w, r, http.StatusUnauthorized, "Sign-in failed", "The ID token from the identity provider could not be verified.")
		return
	}

	roles := cfg.Rules().Evaluate(ident.Claims)
	if s.Linker != nil {
		refused, err := s.Linker.Resolve(ctx, cfg, ident, len(roles) > 0)
		if err != nil {
			s.record(ctx, anonymousActor, target, OutcomeFailed)
			s.fail(w, r, "link the identity", cfg.ID, err)
			return
		}
		if refused != nil {
			s.errorPage(w, r, http.StatusForbidden, refused.Title, refused.Message)
			return
		}
	}
	username, _ := ident.Claims["preferred_username"].(string)
	res, err := s.Store.SyncOIDCLogin(ctx, store.OIDCSyncParams{
		Issuer: ident.Issuer, Subject: ident.Subject, ProviderID: cfg.ID,
		PreferredUsername: username, Roles: roles, CreateIfMissing: len(roles) > 0,
		RefreshToken: tokens.RefreshToken.Reveal(), NowMS: store.UnixMS(s.now()),
		// The role-change audit row commits with the roles it describes.
		OnRolesChanged: func(ctx context.Context, ex store.Execer, res store.OIDCSyncResult) error {
			return s.Audit.RecordTx(ctx, ex, audit.Event{Actor: res.User.ID, ActingAs: "", Action: AuditActionRoleSync,
				Target: res.User.ID, Outcome: OutcomeChanged})
		},
	})
	if err != nil {
		s.record(ctx, anonymousActor, target, OutcomeFailed)
		s.fail(w, r, "apply the sign-in", cfg.ID, err)
		return
	}
	switch {
	case res.Inactive:
		s.record(ctx, res.User.ID, sanitizeTarget(res.User.Username), OutcomeInactive)
		s.errorPage(w, r, http.StatusForbidden, "Account deactivated", "This account is deactivated. Ask an administrator.")
		return
	case len(roles) == 0:
		actor, tgt := anonymousActor, target
		if res.Found {
			actor, tgt = res.User.ID, sanitizeTarget(res.User.Username)
		}
		s.record(ctx, actor, tgt, OutcomeDenied)
		s.errorPage(w, r, http.StatusForbidden, "No access",
			"You signed in at the identity provider, but none of your roles gives access to this Hub. Ask an administrator to add you to a group that has access.")
		return
	}

	ttl, err := s.TTL(ctx)
	if err != nil {
		s.fail(w, r, "session lifetime", cfg.ID, err)
		return
	}
	issued, err := s.Sessions.Create(ctx, res.User, ttl)
	if err != nil {
		s.fail(w, r, "create session", cfg.ID, err)
		return
	}
	// The success row is written before the cookie is handed out: when it
	// cannot be written the session is revoked again and the sign-in fails.
	err = s.Audit.Record(ctx, audit.Event{Actor: res.User.ID, Action: AuditActionLogin, Target: sanitizeTarget(res.User.Username), Outcome: OutcomeSuccess})
	if err != nil {
		_ = s.Sessions.Revoke(context.WithoutCancel(ctx), issued.Token)
		s.fail(w, r, "audit sign-in", cfg.ID, err)
		return
	}
	http.SetCookie(w, session.Cookie(issued, s.Secure(r)))
	http.Redirect(w, r, safeNext(tx.NextPath), http.StatusSeeOther)
}

// safeNext returns next when it is a path on this Hub, and "/" otherwise. A
// scheme, a host, a backslash or a control character make it unsafe: it must
// never send the browser to another site.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\") {
		return "/"
	}
	for _, r := range next {
		if unicode.IsControl(r) {
			return "/"
		}
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

// record audits one sign-in that did not succeed. A failing audit write does
// not change the answer, but it is logged.
func (s *Service) record(ctx context.Context, actor, target, outcome string) {
	s.audit(ctx, actor, target, AuditActionLogin, outcome)
}

func (s *Service) audit(ctx context.Context, actor, target, action, outcome string) {
	err := s.Audit.Record(context.WithoutCancel(ctx), audit.Event{Actor: actor, Action: action, Target: target, Outcome: outcome})
	if err != nil {
		s.logger().Error("audit of an OIDC event failed", "action", action, "outcome", outcome, "error", err)
	}
}

func sanitizeTarget(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// fail logs err and answers with an error page that says what kind of step
// failed. A network or protocol failure is a bad gateway; the page never
// carries the error text, which can name internal addresses.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, step, provider string, err error) {
	s.logger().Error("OIDC sign-in failed", "step", step, "provider", provider, "error", err)
	switch step {
	case "discovery", "code exchange":
		s.errorPage(w, r, http.StatusBadGateway, "Identity provider unreachable",
			"The Hub could not complete the sign-in with the identity provider ("+step+" failed or timed out). Nothing was changed. Try again later or ask your administrator.")
	default:
		s.errorPage(w, r, http.StatusInternalServerError, "Sign-in failed", "The sign-in could not be completed. Nothing was changed.")
	}
}

var errorTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title><link rel="stylesheet" href="/static/minion.css"></head>
<body><main class="card auth-card"><h1>{{.Title}}</h1><p>{{.Message}}</p><p><a class="btn" href="/login">Back to sign in</a></p></main></body></html>
`))

// errorPage renders the English error page of a failed sign-in.
func (s *Service) errorPage(w http.ResponseWriter, _ *http.Request, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := errorTemplate.Execute(w, struct{ Title, Message string }{title, message}); err != nil {
		s.logger().Error("render OIDC error page", "error", err)
	}
}
