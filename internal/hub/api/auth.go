package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// KeyPrefix starts every API key (AD-11).
const KeyPrefix = "mn_"

// maxTokenLen bounds the bearer token that is looked at: a real key is
// "mn_" plus 52 base32 characters.
const maxTokenLen = 128

// ErrInvalidKey is returned by a [KeyAuthenticator] for a token that is
// unknown, expired, revoked or malformed, or whose owner is deactivated. It is
// the only error that counts as a failed attempt; any other error is an
// internal failure.
var ErrInvalidKey = errors.New("api: invalid API key")

// KeyIdentity is the result of a successful key authentication.
type KeyIdentity struct {
	// KeyID is the stored id of the key (never the token or its hash). It is
	// the audit actor of calls made with the key.
	KeyID string
	// Principal is the authz principal of the key (authz.NewAPIKeyPrincipal):
	// the owner, the scope and an optional Worker binding.
	Principal authz.Principal
}

// KeyAuthenticator is the port to the API-key store (Story 6.1). It receives
// the bearer token, finds the key by its SHA-256, and checks expiry and
// revocation. It must not log the token.
type KeyAuthenticator interface {
	AuthenticateKey(ctx context.Context, token string) (KeyIdentity, error)
}

// SessionResolver is the port to the browser sessions; *session.Manager
// implements it.
type SessionResolver interface {
	Resolve(ctx context.Context, token string) (*session.Session, error)
}

var _ SessionResolver = (*session.Manager)(nil)

// Limiter is the part of the shared rate limiter that the API uses;
// *ratelimit.Limiter implements it.
type Limiter interface {
	Allow(policy ratelimit.Policy, ip netip.Addr, target string) (bool, time.Duration)
}

var _ Limiter = (*ratelimit.Limiter)(nil)

// Auditor records audit events; *audit.Log implements it.
type Auditor interface {
	Record(ctx context.Context, ev audit.Event) error
}

var _ Auditor = (*audit.Log)(nil)

// Identity is an authenticated caller.
type Identity struct {
	// Principal is what authz.Decide evaluates.
	Principal authz.Principal
	// Actor is the audit actor: the user id for a session, "key:<id>" for an
	// API key, the admin for act-as.
	Actor string
	// ActingAs is the user an admin acts as (act-as only).
	ActingAs string
	// Via is ViaSession or ViaAPIKey.
	Via string
	// IP is the client address as the Hub sees it.
	IP netip.Addr
	// Session is the browser session (ViaSession only).
	Session *session.Session
}

// How a caller authenticated.
const (
	ViaSession = "session"
	ViaAPIKey  = "api_key"
)

// Audit vocabulary of failed bearer attempts (AD-20). No event holds the
// token or any part of it.
const (
	AuditActionKeyAuth   = "apikey.auth"
	AuditOutcomeFailed   = "failed"
	AuditOutcomeLimited  = "rate_limited"
	auditActorAnonymous  = "anonymous"
	auditTargetUnknownIP = "unknown"
)

// Authenticator turns a request into an [Identity]: a browser session cookie
// with the CSRF header, or "Authorization: Bearer mn_...". When a request
// carries an Authorization header it is judged by that header alone, so a
// stray cookie cannot rescue a bad token.
type Authenticator struct {
	// Sessions resolves the session cookie. Required.
	Sessions SessionResolver
	// Limiter applies the apikey_fail policy per client IP. Nil disables it.
	Limiter Limiter
	// Audit records failed bearer attempts without the token. Nil disables it.
	Audit Auditor
	// ClientIP returns the client address with the trusted-proxy rules of the
	// Hub (server.Network.ClientIP). Nil uses the peer address.
	ClientIP func(*http.Request) netip.Addr
	// Log receives internal failures; nil means slog.Default.
	Log *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	keys    atomic.Pointer[keyBox]
	blocked blocklist
}

type keyBox struct{ k KeyAuthenticator }

// SetKeyAuthenticator wires the API-key port, once. Until then every bearer
// token is invalid, so the Hub starts without the key module and fails
// closed. The API-key module (Story 6.1) calls it from its init.
func (a *Authenticator) SetKeyAuthenticator(k KeyAuthenticator) error {
	if k == nil {
		return errors.New("api: SetKeyAuthenticator needs an authenticator")
	}
	if !a.keys.CompareAndSwap(nil, &keyBox{k: k}) {
		return errors.New("api: the key authenticator is already set")
	}
	return nil
}

func (a *Authenticator) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authenticator) clientIP(r *http.Request) netip.Addr {
	if a.ClientIP != nil {
		return a.ClientIP(r)
	}
	addr, err := ratelimit.ParseRemote(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

func unauthenticated(message string) *Error {
	e := NewError(proto.CodeUnauthenticated, message)
	e.challenge = true
	return e
}

// Authenticate returns the caller of r, or an *Error: unauthenticated,
// forbidden (bad CSRF, or a Worker-bound key), rate_limited or internal.
func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	ip := a.clientIP(r)
	var (
		id  Identity
		err error
	)
	if vals := r.Header.Values("Authorization"); len(vals) > 0 {
		id, err = a.bearer(r, ip, vals)
	} else {
		id, err = a.cookie(r)
	}
	id.IP = ip
	return id, err
}

// safeMethod reports whether a method needs no CSRF token. AD-10 exempts GET
// only ("requests other than GET need the X-Minion-CSRF header bound to the
// session"), so HEAD and OPTIONS are checked like every other method.
func safeMethod(m string) bool {
	return m == http.MethodGet
}

func (a *Authenticator) cookie(r *http.Request) (Identity, error) {
	c, err := r.Cookie(session.CookieName)
	if err != nil || c.Value == "" {
		return Identity{}, unauthenticated("sign in or send an API key")
	}
	// The CSRF check comes before the lookup and does not depend on it: a
	// browser acting on a cookie it cannot prove to own is refused alike
	// whether the cookie is stale or not (same rule as session.Middleware).
	if !safeMethod(r.Method) && !session.ValidCSRF(c.Value, r.Header.Get(session.CSRFHeader)) {
		return Identity{}, NewError(proto.CodeForbidden, "missing or invalid "+session.CSRFHeader+" header")
	}
	s, err := a.Sessions.Resolve(r.Context(), c.Value)
	switch {
	case err == nil:
	case errors.Is(err, session.ErrNoSession):
		return Identity{}, unauthenticated("sign in first")
	default:
		a.logger().Error("session lookup failed", "error", err)
		return Identity{}, internalError()
	}
	p := s.Principal()
	return identityOf(p, ViaSession, p.UserID, s), nil
}

func identityOf(p authz.Principal, via, actor string, s *session.Session) Identity {
	id := Identity{Principal: p, Actor: actor, Via: via, Session: s}
	if p.Kind == authz.KindActAs {
		id.Actor, id.ActingAs = p.ActorID, p.UserID
	}
	return id
}

func (a *Authenticator) bearer(r *http.Request, ip netip.Addr, vals []string) (Identity, error) {
	now := a.now()
	if wait := a.blocked.wait(ip, now); wait > 0 {
		// The address has used up its failed attempts: refuse before looking
		// at the token, so that a flood cannot keep probing keys.
		e := NewError(proto.CodeRateLimited, "too many failed API key attempts")
		e.RetryAfter = wait
		return Identity{}, e
	}
	token, ok := bearerToken(vals)
	if !ok {
		return Identity{}, unauthenticated("the Authorization header must be: Bearer <API key>")
	}
	box := a.keys.Load()
	if box == nil || !strings.HasPrefix(token, KeyPrefix) || len(token) > maxTokenLen {
		return Identity{}, a.failedKey(r.Context(), ip, now)
	}
	ki, err := box.k.AuthenticateKey(r.Context(), token)
	switch {
	case err == nil:
	case errors.Is(err, ErrInvalidKey):
		return Identity{}, a.failedKey(r.Context(), ip, now)
	default:
		a.logger().Error("api key lookup failed", "error", err)
		return Identity{}, internalError()
	}
	p := ki.Principal
	if p.Kind != authz.KindAPIKey {
		// A misbehaving port must not widen access: fail closed.
		a.logger().Error("the key authenticator returned a principal that is not an API key", "kind", string(p.Kind))
		return Identity{}, internalError()
	}
	if p.WorkerID != "" {
		// AD-11: a key bound to a Worker identity is accepted only on that
		// Worker's call streams, never on the REST API.
		return Identity{}, NewError(proto.CodeForbidden, "this API key is bound to a Worker and cannot call the REST API")
	}
	actor := p.UserID
	if ki.KeyID != "" {
		actor = "key:" + ki.KeyID
	}
	return identityOf(p, ViaAPIKey, actor, nil), nil
}

// bearerToken extracts the token of exactly one "Bearer <token>" header.
func bearerToken(vals []string) (string, bool) {
	if len(vals) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(strings.TrimSpace(vals[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

// failedKey counts one failed bearer attempt against the apikey_fail policy
// of the client IP and audits it without the token. It returns the error for
// the caller: unauthenticated, or rate_limited once the budget is gone.
func (a *Authenticator) failedKey(ctx context.Context, ip netip.Addr, now time.Time) *Error {
	allowed, wait := true, time.Duration(0)
	if a.Limiter != nil {
		allowed, wait = a.Limiter.Allow(ratelimit.PolicyAPIKeyFail, ip, "")
	}
	outcome := AuditOutcomeFailed
	if !allowed {
		outcome = AuditOutcomeLimited
		a.blocked.block(ip, now, wait)
	}
	if a.Audit != nil {
		target := auditTargetUnknownIP
		if ip.IsValid() {
			target = ip.String()
		}
		ev := audit.Event{Actor: auditActorAnonymous, Action: AuditActionKeyAuth, Target: target, Outcome: outcome}
		if err := a.Audit.Record(context.WithoutCancel(ctx), ev); err != nil {
			a.logger().Error("audit write failed", "action", AuditActionKeyAuth, "error", err)
		}
	}
	if !allowed {
		e := NewError(proto.CodeRateLimited, "too many failed API key attempts")
		e.RetryAfter = wait
		return e
	}
	return unauthenticated("invalid API key")
}

// maxBlocked bounds the memory of the blocklist.
const maxBlocked = 4096

// blocklist remembers the clients whose apikey_fail budget ran out, until the
// limiter says they may try again. The shared limiter only counts failures
// (Allow takes a token), so without this a client over its limit could still
// get a good token accepted; with it, the client is refused before the token
// is looked at.
type blocklist struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (b *blocklist) block(ip netip.Addr, now time.Time, wait time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until == nil {
		b.until = map[string]time.Time{}
	}
	key := ratelimit.IPKey(ip)
	if _, ok := b.until[key]; !ok && len(b.until) >= maxBlocked {
		for k, t := range b.until {
			if !t.After(now) {
				delete(b.until, k)
			}
		}
		for k := range b.until { // still full: the limiter keeps protecting
			delete(b.until, k)
			break
		}
	}
	b.until[key] = now.Add(wait)
}

// wait returns how long the client must still wait, or zero.
func (b *blocklist) wait(ip netip.Addr, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.until) == 0 {
		return 0
	}
	key := ratelimit.IPKey(ip)
	t, ok := b.until[key]
	if !ok {
		return 0
	}
	if !t.After(now) {
		delete(b.until, key)
		return 0
	}
	return t.Sub(now)
}
