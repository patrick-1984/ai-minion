package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Re-authentication of a signed-in session through the identity provider
// (docs/architecture/proposals/hub-identity-v2.md, section 8.1 item 2).
//
// The Hub starts a new authorization request with max_age=0 and prompt=login
// and keeps a one-time state, nonce and PKCE verifier bound to the session. The
// callback verifies the ID token as a sign-in does and, in addition, requires
// an auth_time claim that is not earlier than the moment the request started
// (with [authTimeSkew] of clock allowance) and the same subject as the
// session. Only then are auth_at and auth_method of that same session updated.
// Every other outcome is reauth_unavailable and updates nothing: a provider
// that returns no auth_time, ignores max_age, returns another subject, or a
// state that is unknown, expired or used is never a fresh credential check.

// ReauthStartPath is the endpoint that starts a re-authentication.
const ReauthStartPath = "/auth/reauth/oidc"

// ReauthResultPath is the page the callback sends the browser to; the query
// parameter reauth is "ok" or "unavailable".
const ReauthResultPath = "/admin/identity"

// AuditActionReauthFailed is the audit action of a failed re-authentication.
const AuditActionReauthFailed = "hub_identity.reauth_failed"

const (
	// authTimeSkew is the clock allowance around auth_time.
	authTimeSkew = 60 * time.Second
	// reauthTTL is how long a started re-authentication may wait.
	reauthTTL = 10 * time.Minute
	// maxReauthPending bounds the table.
	maxReauthPending = 256
)

type reauthTx struct {
	sessionID, userID, providerID string
	issuer, subject               string
	nonce, verifier, bindingHash  string
	startedAt, expiresAt          time.Time
}

type reauthTable struct {
	mu   sync.Mutex
	m    map[string]reauthTx // by state hash
	used map[string]usedReauth
}

// usedReauth remembers a consumed state for reauthTTL so that a replay is
// recognized as a re-authentication and answered as one.
type usedReauth struct {
	userID  string
	expires time.Time
}

// reauthCookiePrefix marks the binding cookie of a re-authentication, so a
// callback whose state is unknown (restart, expiry) is still recognized.
const reauthCookiePrefix = "r."

func (t *reauthTable) put(stateHash string, tx reauthTx, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]reauthTx{}
	}
	for k, v := range t.m {
		if !now.Before(v.expiresAt) || v.sessionID == tx.sessionID {
			delete(t.m, k) // expired, or superseded by a newer start of the same session
		}
	}
	if len(t.m) >= maxReauthPending {
		return false
	}
	t.m[stateHash] = tx
	return true
}

// take removes and returns the transaction of a state: one use. An expired
// one is returned as found (so the callback is consumed and answered as
// unavailable) with the zero expiry check left to the caller.
func (t *reauthTable) take(stateHash string, _ time.Time) (reauthTx, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tx, ok := t.m[stateHash]
	if ok {
		delete(t.m, stateHash)
		if t.used == nil {
			t.used = map[string]usedReauth{}
		}
		now := time.Now()
		for k, v := range t.used {
			if now.After(v.expires) {
				delete(t.used, k)
			}
		}
		if len(t.used) < 4*maxReauthPending {
			t.used[stateHash] = usedReauth{userID: tx.userID, expires: now.Add(reauthTTL)}
		}
	}
	return tx, ok
}

// wasUsed reports whether stateHash is a consumed re-authentication state.
func (t *reauthTable) wasUsed(stateHash string) (userID string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	u, ok := t.used[stateHash]
	if !ok || time.Now().After(u.expires) {
		return "", false
	}
	return u.userID, true
}

// unknownReauth answers a callback whose state is not in the table when it is
// a replayed re-authentication state or comes from a browser that started a
// re-authentication (marked binding cookie): reauth_unavailable and the audit
// event hub_identity.reauth_failed, nothing updated. It reports whether it
// answered.
func (s *Service) unknownReauth(w http.ResponseWriter, r *http.Request, stateHash string) bool {
	userID, replay := s.reauth.wasUsed(stateHash)
	if !replay {
		c, err := r.Cookie(BindingCookie)
		if err != nil || !strings.HasPrefix(c.Value, reauthCookiePrefix) {
			return false
		}
		userID = anonymousActor
	}
	reason := "unknown_state"
	if replay {
		reason = "replay"
	}
	s.audit(r.Context(), userID, "method=oidc reason="+reason, AuditActionReauthFailed, OutcomeFailed)
	http.Redirect(w, r, ReauthResultPath+"?reauth=unavailable", http.StatusSeeOther)
	return true
}

// loginAuth is the authentication a new OIDC session starts with: the verified
// auth_time claim of the ID token when it has a plausible one. A session
// without it records only the method and has auth_at 0, so it is never fresh.
func (s *Service) loginAuth(providerID string, claims map[string]any) session.Auth {
	at, ok := authTime(claims)
	now := s.now()
	if !ok || at.After(now.Add(authTimeSkew)) {
		return session.Auth{Method: session.AuthMethodOIDCPrefix + providerID}
	}
	if at.After(now) {
		at = now
	}
	return session.Auth{At: at, Method: session.AuthMethodOIDCPrefix + providerID}
}

// authTime reads the auth_time claim (seconds since the epoch, a JSON number).
func authTime(claims map[string]any) (time.Time, bool) {
	var secs float64
	switch v := claims["auth_time"].(type) {
	case float64:
		secs = v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return time.Time{}, false
		}
		secs = f
	default:
		return time.Time{}, false
	}
	if math.IsNaN(secs) || math.IsInf(secs, 0) || secs <= 0 || secs > 1<<40 {
		return time.Time{}, false
	}
	return time.Unix(int64(secs), 0).UTC(), true
}

type reauthStartResponse struct {
	RedirectURL string `json:"redirect_url"`
}

// ReauthStart handles POST /auth/reauth/oidc for the signed-in session: it
// records the one-time state bound to the session and answers the URL of the
// identity provider that the browser must open.
func (s *Service) ReauthStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess, ok := session.FromContext(r.Context())
	if !ok {
		session.WriteError(w, proto.CodeUnauthenticated, "sign in first", nil)
		return
	}
	unavailable := func(msg string) {
		s.audit(ctx, sess.UserID, "method=oidc reason=start", AuditActionReauthFailed, OutcomeFailed)
		session.WriteError(w, proto.CodeReauthUnavailable, msg, nil)
	}
	providerID, isOIDC := cutPrefix(sess.AuthMethod, session.AuthMethodOIDCPrefix)
	if sess.ActAs != nil || !isOIDC || providerID == "" {
		unavailable("this session did not sign in through an identity provider; re-authenticate with your password")
		return
	}
	if ok, wait := s.allow(r); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait/time.Second)+1))
		session.WriteError(w, proto.CodeRateLimited, "too many attempts; retry later", nil)
		return
	}
	cfg, err := s.find(ctx, providerID)
	if err != nil {
		unavailable("the identity provider of this session is not available")
		return
	}
	issuer, subject, err := s.Store.ExternalIdentityOf(ctx, sess.UserID, providerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			unavailable("the identity provider has no identity for this account")
			return
		}
		s.logger().Error("OIDC re-authentication: read the identity", "error", err)
		session.WriteError(w, proto.CodeInternal, "the request could not be completed", nil)
		return
	}
	prov, err := s.provider(ctx, cfg)
	if err != nil {
		s.logger().Error("OIDC re-authentication: discovery", "provider", providerID, "error", err)
		unavailable("the identity provider could not be reached")
		return
	}
	state, err1 := randomString(32)
	nonce, err2 := randomString(32)
	bindingRand, err3 := randomString(32)
	binding := reauthCookiePrefix + bindingRand
	if err := errors.Join(err1, err2, err3); err != nil {
		s.logger().Error("OIDC re-authentication: random values", "error", err)
		session.WriteError(w, proto.CodeInternal, "the request could not be completed", nil)
		return
	}
	verifier := oauth2.GenerateVerifier()
	now := s.now()
	tx := reauthTx{
		sessionID: sess.ID, userID: sess.UserID, providerID: providerID, issuer: issuer, subject: subject,
		nonce: nonce, verifier: verifier, bindingHash: store.HashValue(binding),
		startedAt: now, expiresAt: now.Add(reauthTTL),
	}
	if !s.reauth.put(store.HashValue(state), tx, now) {
		w.Header().Set("Retry-After", "60")
		session.WriteError(w, proto.CodeRateLimited, "too many re-authentications are waiting; retry later", nil)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: BindingCookie, Value: binding, Path: "/auth/oidc",
		MaxAge: int(reauthTTL / time.Second), HttpOnly: true,
		Secure: s.Secure(r), SameSite: http.SameSiteLaxMode,
	})
	session.WriteJSON(w, http.StatusOK, reauthStartResponse{RedirectURL: prov.AuthCodeURLReauth(s.redirectURL(r), state, nonce, verifier)})
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// reauthCallback finishes a re-authentication whose state matched. Every
// failure is audited, updates nothing and sends the browser back to the
// identity page with reauth=unavailable.
func (s *Service) reauthCallback(w http.ResponseWriter, r *http.Request, tx reauthTx) {
	ctx := r.Context()
	back := func(result string) {
		http.Redirect(w, r, ReauthResultPath+"?reauth="+result, http.StatusSeeOther)
	}
	fail := func(reason string) {
		s.audit(ctx, tx.userID, "method=oidc reason="+reason, AuditActionReauthFailed, OutcomeFailed)
		back("unavailable")
	}
	now := s.now()
	if !now.Before(tx.expiresAt) {
		fail("expired")
		return
	}
	c, err := r.Cookie(BindingCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(store.HashValue(c.Value)), []byte(tx.bindingHash)) != 1 {
		fail("browser")
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" || q.Get("code") == "" {
		fail("provider")
		return
	}
	cfg, err := s.find(ctx, tx.providerID)
	if err != nil {
		fail("provider")
		return
	}
	prov, err := s.provider(ctx, cfg)
	if err != nil {
		s.logger().Error("OIDC re-authentication: discovery", "provider", tx.providerID, "error", err)
		fail("provider")
		return
	}
	tokens, err := prov.Exchange(ctx, s.redirectURL(r), q.Get("code"), tx.verifier)
	if err != nil {
		s.logger().Error("OIDC re-authentication: code exchange", "provider", tx.providerID, "error", err)
		fail("exchange")
		return
	}
	ident, err := prov.VerifyIDToken(ctx, tokens.IDToken, tx.nonce)
	if err != nil {
		s.logger().Warn("OIDC re-authentication: ID token refused", "provider", tx.providerID, "error", err)
		fail("token")
		return
	}
	if ident.Issuer != tx.issuer || ident.Subject != tx.subject {
		fail("subject")
		return
	}
	at, ok := authTime(ident.Claims)
	if !ok {
		fail("auth_time")
		return
	}
	if at.Before(tx.startedAt.Add(-authTimeSkew)) || at.Before(now.Add(-session.ReauthWindow)) || at.After(now.Add(authTimeSkew)) {
		fail("not_fresh")
		return
	}
	if err := s.markSession(ctx, tx, at); err != nil {
		if !errors.Is(err, session.ErrNoSession) {
			s.logger().Error("OIDC re-authentication: record the authentication", "error", err)
		}
		fail("session")
		return
	}
	back("ok")
}

// markSession stores the verified auth_time of the ID token as the session's
// auth_at, not the time of the callback; a time a little ahead of the Hub's
// clock (within the skew) is clamped to now.
func (s *Service) markSession(ctx context.Context, tx reauthTx, at time.Time) error {
	_, err := s.Sessions.MarkAuthenticatedAt(ctx, tx.sessionID, session.AuthMethodOIDCPrefix+tx.providerID, at)
	return err
}
