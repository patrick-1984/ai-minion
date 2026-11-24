package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

type ctxKey struct{}

// FromContext returns the session that [Middleware.Wrap] attached to the
// request context.
func FromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(*Session)
	return s, ok && s != nil
}

// WithSession returns ctx carrying s. [Middleware.Wrap] uses it; tests of
// handlers behind the middleware use it too.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// BackgroundHeader marks a request that the page sends on its own (a timed
// poll, a refresh after a live event), not because the person did something.
// Such a request never extends the idle lifetime of the session and never
// renews it with the identity provider, and a session whose idle lifetime
// ran out answers it with a quiet 401 without a redirect (see [Idle]), so the
// screen stays as it is.
const BackgroundHeader = "X-Minion-Background"

// IdleHeader is set to "idle" on the quiet 401 that a background request of a
// session whose idle lifetime ran out gets: the page pauses (keeps what it
// holds) instead of treating it as a sign-out. The API error carries the same
// as details.reason [IdleReason].
const IdleHeader = "X-Minion-Session"

// IdleReason is the details.reason of the API error for a background request
// of a session whose idle lifetime ran out.
const IdleReason = "session_idle"

type backgroundKey struct{}

type idleKey struct{}

// AsBackground returns ctx marked so that a request carrying it is
// [Background]. Recurring authorization checks of a stream use it: they are
// not the person acting.
func AsBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// Background reports whether r is a background request: it carries
// [BackgroundHeader] "1", it opens an event stream (a live page reconnecting
// its stream is not the person acting), or its context was marked with
// [AsBackground].
func Background(r *http.Request) bool {
	if v, _ := r.Context().Value(backgroundKey{}).(bool); v {
		return true
	}
	return r.Header.Get(BackgroundHeader) == "1" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// WithIdle returns ctx marked as [Idle]. [Middleware.Wrap] uses it; tests of
// handlers behind the middleware use it too.
func WithIdle(ctx context.Context) context.Context {
	return context.WithValue(ctx, idleKey{}, true)
}

// Idle reports whether the request context records that the request carried
// the cookie of a session whose idle lifetime ran out, and was not allowed to
// renew it because it is [Background]. [Middleware.Wrap] sets it.
func Idle(ctx context.Context) bool {
	v, _ := ctx.Value(idleKey{}).(bool)
	return v
}

// CrossOrigin reports whether a browser says that another origin caused r:
// Sec-Fetch-Site is "cross-site" or "same-site" (a sibling origin), or, from a
// browser that sends no Sec-Fetch-Site, an Origin header that does not name
// the request's own host. Sec-Fetch-Site "same-origin" and "none" (the person
// typed the address, used a bookmark or reloaded) are not cross-origin. A
// request without either header (curl, other programs) is not either.
func CrossOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "":
	default:
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return true // "null" and other opaque origins
	}
	return !strings.EqualFold(u.Host, r.Host)
}

// Foreground reports whether r counts as the person's own activity: it is not
// [Background] and not [CrossOrigin]. Only a foreground request extends the
// idle lifetime of a session or renews an OIDC session with the identity
// provider.
func Foreground(r *http.Request) bool {
	return !Background(r) && !CrossOrigin(r)
}

// RenewOutcome is the answer of a [Renewer].
type RenewOutcome int

const (
	// RenewFailed means the identity provider no longer vouches for the user
	// (a revoked or rejected refresh token, a deactivated account, no role
	// left, no refresh token stored): the session ends.
	RenewFailed RenewOutcome = iota
	// RenewOK means the identity provider vouched for the user again with
	// fresh claims and the roles still give access: the session goes on.
	RenewOK
	// RenewUnavailable means nothing was learned (the identity provider could
	// not be reached): the session is served within [RenewGrace].
	RenewUnavailable
)

// Renewer re-validates the user of an expired OIDC session with the identity
// provider. The OIDC refresher implements it; it records the audit rows.
type Renewer interface {
	RenewSession(ctx context.Context, s *Session) RenewOutcome
}

// renewRetry is how long after an unreachable identity provider the next
// renewal of the same session is attempted; requests in between are served
// within the grace without asking the provider again.
const renewRetry = 30 * time.Second

// renewTimeout bounds one renewal, whatever the request's own context does:
// a refresh that the provider answered must still be stored when the browser
// went away. The renewer stores a rotated token with its own bounded context
// even after this ran out.
const renewTimeout = 30 * time.Second

// Middleware authenticates browser sessions and enforces CSRF protection.
type Middleware struct {
	Manager *Manager
	// Log receives store failures; nil means [slog.Default].
	Log *slog.Logger
	// Renew, when set, renews an OIDC session whose idle lifetime ran out
	// (see [Middleware.Wrap]). Nil means an expired session is signed out.
	Renew Renewer

	mu      sync.Mutex
	locks   map[string]*renewLock
	backoff map[string]time.Time
}

type renewLock struct {
	mu   sync.Mutex
	refs int
}

// lockRenew serialises the renewals of one session: concurrent requests of
// the same browser wait for the first one, and then find the session renewed.
func (mw *Middleware) lockRenew(id string) func() {
	mw.mu.Lock()
	if mw.locks == nil {
		mw.locks = map[string]*renewLock{}
	}
	l := mw.locks[id]
	if l == nil {
		l = &renewLock{}
		mw.locks[id] = l
	}
	l.refs++
	mw.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		mw.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(mw.locks, id)
		}
		mw.mu.Unlock()
	}
}

func (mw *Middleware) backoffUntil(id string) time.Time {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	return mw.backoff[id]
}

func (mw *Middleware) setBackoff(id string, until time.Time) {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if mw.backoff == nil {
		mw.backoff = map[string]time.Time{}
	}
	if until.IsZero() {
		delete(mw.backoff, id)
		return
	}
	now := mw.Manager.Now()
	for k, t := range mw.backoff { // keep the map small: drop what is over
		if !t.After(now) {
			delete(mw.backoff, k)
		}
	}
	mw.backoff[id] = until
}

func (mw *Middleware) logger() *slog.Logger {
	if mw.Log != nil {
		return mw.Log
	}
	return slog.Default()
}

// safeMethod reports whether a request with this method is exempt from the
// CSRF check. AD-10 exempts GET only: every other method, HEAD and OPTIONS
// included, needs the X-Minion-CSRF header bound to the session.
func safeMethod(m string) bool {
	return m == http.MethodGet
}

// Wrap returns a handler that
//
//  1. rejects with forbidden every request other than GET that carries a
//     session cookie but not the matching X-Minion-CSRF header
//     (a cookie that is stale or malformed gets the same answer: the browser
//     is acting on a session it cannot prove to own);
//  2. resolves the cookie and attaches the [Session] to the request context
//     when it is valid. A request without a valid session goes on anonymously;
//     use [Middleware.Require] to refuse it;
//  3. records activity: a [Foreground] request moves the idle end of the
//     session ([Manager.Touch]); a background or cross-origin one never does;
//  4. renews an OIDC session whose idle lifetime ran out, when [Middleware.Renew]
//     is set and the request is [Foreground]: the identity provider is asked
//     with the stored refresh token, and when it vouches for the user the
//     session goes on with the same cookie and the request is served as if it
//     had never expired. Only a transient failure (the provider or its keys
//     could not be reached) is tolerated, for [RenewGrace] after the idle end;
//     a refusal signs the session out. A background request goes on
//     anonymously with [Idle] set in its context: it gets a quiet 401; a
//     cross-origin one goes on anonymously.
//
// A store failure answers internal, never "anonymous".
func (mw *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !safeMethod(r.Method) && !ValidCSRF(c.Value, r.Header.Get(CSRFHeader)) {
			WriteError(w, proto.CodeForbidden, "missing or invalid "+CSRFHeader+" header", nil)
			return
		}
		s, err := mw.ResolveRequest(r, c.Value)
		var expired *ExpiredError
		switch {
		case err == nil:
			r = r.WithContext(WithSession(r.Context(), s))
		case errors.As(err, &expired):
			// anonymous, idle: a background request of a session whose idle
			// lifetime ran out
			r = r.WithContext(WithIdle(r.Context()))
		case errors.Is(err, ErrNoSession):
			// anonymous
		default:
			mw.logger().Error("session lookup failed", "error", err)
			WriteError(w, proto.CodeInternal, "the session could not be checked", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Resolve is [Manager.Resolve]: it judges the cookie value alone, without
// recording activity or renewing.
func (mw *Middleware) Resolve(ctx context.Context, token string) (*Session, error) {
	return mw.Manager.Resolve(ctx, token)
}

// ResolveRequest resolves the session cookie value token of the request r as
// [Middleware.Wrap] does: a [Foreground] request records activity
// ([Manager.Touch]), and renews an expired OIDC session when
// [Middleware.Renew] is set. It returns [ErrNoSession] when the request has no
// valid session; for a [Background] request that carries a session whose
// idle lifetime ran out the error is an *[ExpiredError] (which matches
// ErrNoSession), so that the caller can answer with the quiet idle 401. Any
// other error is a store failure. The REST API's cookie authentication uses
// it, so that a form posting JSON behaves as a page does.
func (mw *Middleware) ResolveRequest(r *http.Request, token string) (*Session, error) {
	fg := Foreground(r)
	s, err := mw.Manager.Resolve(r.Context(), token)
	var expired *ExpiredError
	switch {
	case err == nil:
		if fg {
			if terr := mw.Manager.Touch(r.Context(), s); terr != nil {
				// The request is still valid; the next one tries again.
				mw.logger().Warn("session activity not recorded", "error", terr)
			}
		}
		return s, nil
	case errors.As(err, &expired):
		if !fg {
			// Background polls, streams and cross-origin requests never
			// renew: only the person's own next request does. A background
			// request learns that the session is idle (a quiet 401 that
			// keeps the screen); a cross-origin one is simply signed out.
			if Background(r) {
				return nil, expired
			}
			return nil, ErrNoSession
		}
		if rs := mw.renew(r, token); rs != nil {
			return rs, nil
		}
		return nil, ErrNoSession
	default:
		return nil, err
	}
}

// renew tries to renew the expired session of the foreground request r. It
// returns the session to serve the request with, or nil for "signed out".
func (mw *Middleware) renew(r *http.Request, token string) *Session {
	if mw.Renew == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), renewTimeout)
	defer cancel()
	// Read the session again under the lock: another request of the same
	// browser may have renewed it meanwhile.
	s, err := mw.Manager.Resolve(ctx, token)
	var exp *ExpiredError
	switch {
	case err == nil:
		return s
	case errors.As(err, &exp):
	default:
		return nil // gone, or a store failure: signed out
	}
	if !strings.HasPrefix(exp.Session.AuthMethod, AuthMethodOIDCPrefix) {
		return nil
	}
	id := exp.Session.ID
	unlock := mw.lockRenew(id)
	defer unlock()
	s, err = mw.Manager.Resolve(ctx, token)
	switch {
	case err == nil:
		return s
	case errors.As(err, &exp):
	default:
		return nil
	}

	outcome := RenewUnavailable
	if !mw.Manager.Now().Before(mw.backoffUntil(id)) {
		outcome = mw.Renew.RenewSession(ctx, &exp.Session)
	}
	// The provider call may have taken a while: the session, the account and
	// its credentials are read again, and the deadlines are judged with the
	// clock of now. A password reset, a deactivation or a sign-out during the
	// call is seen here.
	cur, err := mw.Manager.Resolve(ctx, token)
	var again *ExpiredError
	switch {
	case err == nil:
		mw.setBackoff(id, time.Time{})
		return cur // renewed meanwhile (or by this call, below it is stored)
	case errors.As(err, &again):
	default:
		if !errors.Is(err, ErrNoSession) {
			mw.logger().Error("session lookup after a renewal failed", "session", id, "error", err)
		}
		return nil
	}
	switch outcome {
	case RenewOK:
		mw.setBackoff(id, time.Time{})
		if err := mw.Manager.Renewed(ctx, &again.Session); err != nil {
			mw.logger().Error("session renewal could not be stored", "session", id, "error", err)
			return nil
		}
		s, err := mw.Manager.Resolve(ctx, token)
		if err != nil {
			return nil
		}
		return s
	case RenewUnavailable:
		now := mw.Manager.Now()
		if now.Before(again.Session.ExpiresAt.Add(RenewGrace)) && now.Before(again.Session.EndsAt) {
			if !now.Before(mw.backoffUntil(id)) {
				mw.setBackoff(id, now.Add(renewRetry))
			}
			mw.logger().Warn("identity provider unreachable; the expired session is served within the grace", "session", id)
			return &again.Session
		}
		mw.setBackoff(id, time.Time{})
		return nil
	default:
		mw.setBackoff(id, time.Time{})
		if err := mw.Manager.Revoke(ctx, token); err != nil {
			mw.logger().Error("session could not be ended after a refused renewal", "session", id, "error", err)
		}
		return nil
	}
}

// Require returns a handler that answers unauthenticated unless the request
// context holds a session. Put it inside [Middleware.Wrap].
func (mw *Middleware) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			WriteError(w, proto.CodeUnauthenticated, "sign in first", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Protect is Wrap followed by Require.
func (mw *Middleware) Protect(next http.Handler) http.Handler {
	return mw.Wrap(mw.Require(next))
}

// errorBody is the AD-18 error envelope.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    proto.Code     `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// WriteError writes the AD-18 error envelope {"error":{"code","message",
// "details"}} with the HTTP status of code. details may be nil.
func WriteError(w http.ResponseWriter, code proto.Code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	WriteJSON(w, proto.HTTPStatus(code), errorBody{Error: errorDetail{Code: code, Message: message, Details: details}})
}

// WriteJSON writes v as a JSON response with the given status. Auth
// responses are never cached.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
